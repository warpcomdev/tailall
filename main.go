package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akamensky/argparse"
	"github.com/nxadm/tail"
	log "github.com/sirupsen/logrus"

	// Bring profiling interface in
	"net/http"
	_ "net/http/pprof"
)

// MAX_BUFFER Is the maximum number of lines that can be merged together
const MAX_BUFFER = 256

// GRACEFUL_SHUTDOWN is the max number of seconds to wait before exiting the pprof server
const GRACEFUL_SHUTDOWN = 30

// File event interfaces.
// Monitor File addition or removal.

// FileEventType enumerates supported file events
type FileEventType int

const (
	// File existing at start of program
	FILE_EXISTING FileEventType = iota + 1
	// File deleted during program execution
	FILE_DELETED
	// File created during program execution
	FILE_CREATED
)

// FileEvent encapsulates each possible file event
type FileEvent struct {
	Path string
	Type FileEventType
}

type folderScanner struct {
	Folder  string
	Events  chan FileEvent
	visited map[string]bool
}

func (f *folderScanner) deliver(ctx context.Context, filename string, etype FileEventType) (cancelled bool) {
	select {
	case <-ctx.Done():
		return true
	case f.Events <- FileEvent{Path: filepath.Join(f.Folder, filename), Type: etype}:
		return false
	}
}

// Scan folder and generate events for each new file found, or visited file missing
func (f *folderScanner) scanOnce(ctx context.Context, etype FileEventType) (cancelled bool, err error) {
	direntry, err := ioutil.ReadDir(f.Folder)
	if err != nil {
		return false, err
	}
	for k, _ := range f.visited {
		f.visited[k] = false
	}
	for _, info := range direntry {
		if !info.IsDir() {
			infoName := info.Name()
			if _, ok := f.visited[infoName]; !ok {
				log.WithFields(log.Fields{"filename": infoName, "folder": f.Folder}).Debug("Detected file")
				if cancelled = f.deliver(ctx, infoName, etype); cancelled {
					return true, nil
				}
			}
			f.visited[infoName] = true
		}
	}
	for k, v := range f.visited {
		if v == false {
			log.WithFields(log.Fields{"filename": k, "folder": f.Folder}).Debug("Detected removal of file")
			if cancelled = f.deliver(ctx, k, FILE_DELETED); cancelled {
				return true, nil
			}
			delete(f.visited, k)
		}
	}
	return false, nil
}

// Scan a folder and generate events each time a file is created or removed
func Scan(ctx context.Context, folder string, events chan FileEvent) error {
	scanner := folderScanner{
		Folder:  folder,
		Events:  events,
		visited: make(map[string]bool),
	}
	if cancelled, err := scanner.scanOnce(ctx, FILE_EXISTING); cancelled || err != nil {
		return err
	}
	// Subsequent scans: read from start
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if cancelled, err := scanner.scanOnce(ctx, FILE_CREATED); cancelled || err != nil {
				return err
			}
		}
	}
}

type Collector struct {
	// LogStart identifies log line starts
	LogStart string
	// Skip lines containing this pattern
	Skip []string
}

// Keep the given line?
func (c *Collector) Keep(lineStr string) bool {
	if c.Skip != nil {
		for _, skip := range c.Skip {
			if strings.Contains(lineStr, skip) {
				return false
			}
		}
	}
	return true
}

// flush the buffer and empty it before returning
func (c *Collector) flush(lines chan string, buffer []string) []string {
	if len(buffer) > 0 {
		if c.Keep(buffer[0]) {
			lines <- strings.Join(buffer, " ")
		}
	}
	return buffer[:0]
}

/// Input read lines from file and writes them to channel
func (c *Collector) input(ctx context.Context, lines chan string, filename string, seek_end bool) error {
	log.Debug("Entering input goroutine for file ", filename)
	defer log.Debug("Exiting input goroutine for file ", filename)
	whence := os.SEEK_END
	if !seek_end {
		whence = os.SEEK_SET
	}
	t, err := tail.TailFile(filename, tail.Config{
		Follow: true,
		ReOpen: true,
		Logger: tail.DiscardingLogger,
		Location: &tail.SeekInfo{
			Offset: 0,
			Whence: whence,
		},
	})
	if err != nil {
		return err
	}
	// OJO!!!! Solo llamar a t.Cleanup si no se va a volver a reabrir
	// el fichero. En otro caso, mejor llamar a t.Stop.
	// defer t.Cleanup()
	defer t.Stop()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	buffer := make([]string, 0, MAX_BUFFER)
	defer c.flush(lines, buffer)

	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-t.Lines:
			if !ok {
				return nil
			}
			if line.Err != nil {
				return line.Err
			}
			lineStr := line.Text
			if lineStr == "" {
				break
			}
			switch {
			case c.LogStart == "":
				// No prefix to merge, just flush line
				if c.Keep(lineStr) {
					lines <- lineStr
				}
				break
			case strings.HasPrefix(lineStr, c.LogStart) || len(buffer) >= MAX_BUFFER:
				buffer = c.flush(lines, buffer)
				fallthrough
			default:
				buffer = append(buffer, strings.TrimSpace(lineStr))
			}
		case <-ticker.C:
			buffer = c.flush(lines, buffer)
		}
	}
}

/// Output reads lines from channel and writes them to stdout
func (c *Collector) output(lines chan string) {
	log.Debug("Entering output goroutine")
	defer log.Debug("Exiting output goroutine")
	for line := range lines {
		b := []byte(line)
		for start, end := 0, len(b); start < end; {
			n, err := os.Stderr.Write(b[start:end])
			if err != nil {
				break
			}
			start = start + n
		}
		os.Stderr.Write([]byte("\n"))
	}
}

func (c *Collector) Monitor(ctx context.Context, events chan FileEvent) error {

	// Spawn the output task
	wait_output := sync.WaitGroup{}
	lines := make(chan string, MAX_BUFFER)
	wait_output.Add(1)
	go func() {
		c.output(lines)
		wait_output.Done()
	}()

	// Start processing events
	wait_input := sync.WaitGroup{}
	fail := make(chan error, 1)
	failContext, cancel := context.WithCancel(ctx)

	defer func() {
		cancel()          // Cancel input threads first
		wait_input.Wait() // Wait for them to end
		close(lines)      // Close the channels where input threads write
		close(fail)
		wait_output.Wait() // Wait for output thread to end
	}()

	for {
		select {
		case newFile, ok := <-events:
			if !ok {
				// Exit on closed event channel
				return nil
			}
			// Ignore FILE_DELETED for now
			if newFile.Type == FILE_DELETED {
				log.WithField("filename", newFile.Path).Error("Unexpected removal of file")
				break
			}
			seekEnd := false
			if newFile.Type == FILE_EXISTING {
				seekEnd = true
			}
			wait_input.Add(1)
			go func() {
				defer wait_input.Done()
				if err := c.input(failContext, lines, newFile.Path, seekEnd); err != nil {
					// Fail early in case of error, leave backoff to Kubernetes
					select {
					case <-failContext.Done():
						break
					case fail <- err:
						break
					}
				}
			}()
		case <-ctx.Done():
			return nil
		case err := <-fail:
			return err
		}
	}
}

// localIP returns the non loopback local IP of the host
func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addrs {
		// check the address type and if it is not a loopback the display it
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

func main() {

	parser := argparse.NewParser("tailall", "Tails several files in a folder")
	// Create folder flag
	folder := parser.String("f", "folder", &argparse.Options{Required: true, Help: "Path to log folder"})
	logStart := parser.String("l", "logstart", &argparse.Options{Required: false, Help: "Merge lines starting with this prefix"})
	debug := parser.Flag("d", "debug", &argparse.Options{Required: false, Help: "Enabled debug trace"})
	skip := parser.StringList("s", "skip", &argparse.Options{Required: false, Help: "Skip lines containing this text (use %IP% for local IP)"})
	profile := parser.Int("p", "profile", &argparse.Options{Required: false, Help: "Enable profiling listening in this port"})
	// Parse input
	if err := parser.Parse(os.Args); err != nil {
		panic(err)
	}

	log.SetFormatter(&log.TextFormatter{
		DisableColors: true,
		FullTimestamp: true,
	})
	if debug != nil && *debug {
		log.SetLevel(log.TraceLevel)
	}

	logstartParam := ""
	if logStart != nil {
		logstartParam = *logStart
	}

	skipParam := []string{}
	if skip != nil {
		skipParam = *skip
		if ip := localIP(); ip != "" {
			log.Info(fmt.Sprintf("Replacing %%IP%% with %s in skip strings", ip))
			for i := 0; i < len(skipParam); i++ {
				skipParam[i] = strings.ReplaceAll(skipParam[i], "%IP%", ip)
			}
		}
	}

	profileParam := 0
	if profile != nil {
		profileParam = *profile
		if profileParam > 0 && profileParam <= 1024 {
			log.Fatal(fmt.Sprintf("Cannot listen in port %d - only ports > 1024 allowed", *profile))
			profileParam = 0
		}
	}

	folderParam := *folder

	// Cancellation management: context and task groups.
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	scanCtx, scanCancel := context.WithCancel(monitorCtx)
	tasks := sync.WaitGroup{}
	events := make(chan FileEvent, MAX_BUFFER)

	// Start profile server
	if profileParam > 0 {
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			log.WithField("port", profileParam).Debug("Starting profile service")
			srv := &http.Server{Addr: fmt.Sprintf(":%d", profileParam)}
			go srv.ListenAndServe()
			<-scanCtx.Done()
			deadline, _ := context.WithDeadline(context.Background(), time.Now().Add(GRACEFUL_SHUTDOWN*time.Second))
			log.Debug("Shutting down profiling server gracefully")
			srv.Shutdown(deadline)
		}()
	}

	// Start monitor
	logCollector := Collector{LogStart: logstartParam, Skip: skipParam}
	tasks.Add(1)
	go func() {
		defer tasks.Done()
		defer scanCancel() // If logging fails early, cancel scanning too
		if err := logCollector.Monitor(monitorCtx, events); err != nil {
			log.WithError(err).WithField("folder", folderParam).Error("Failed to monitor files")
		}
	}()

	// Start file scanner
	tasks.Add(1)
	go func() {
		defer tasks.Done()
		defer close(events) // If scanning fails, this channel will close and monitor will end
		if err := Scan(scanCtx, folderParam, events); err != nil {
			log.WithError(err).WithField("folder", folderParam).Error("Failed to scan folder")
		}
		log.WithField("folder", folderParam).Error("Exiting folder scanner")
	}()

	// Wait for all tasks to finish
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Warn("Exiting application on received signal")
	monitorCancel()
	tasks.Wait()
}
