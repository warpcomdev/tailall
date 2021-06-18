package main

import (
	"context"
	"io/ioutil"
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
)

const MAXBUFFER = 256

type collector struct {
	// Lines is the flow of input lines
	Lines chan string
	// LogStart identifies log line starts
	LogStart string
	// Skip lines containing this pattern
	Skip []string
}

type eventType int

const (
	FILE_EXISTING eventType = iota + 1
	FILE_DELETED
	FILE_CREATED
)

type newFile struct {
	// Close this particular watcher
	Filename string
	// freshly scanned
	EventType eventType
}

func scanFolder(done chan struct{}, folder string) chan newFile {
	visited := make(map[string]bool)
	filehit := make(chan newFile)
	scan := func(etype eventType) {
		direntry, err := ioutil.ReadDir(folder)
		if err != nil {
			panic(err)
		}
		for k, _ := range visited {
			visited[k] = false
		}
		for _, info := range direntry {
			if !info.IsDir() {
				infoName := info.Name()
				if _, ok := visited[infoName]; !ok {
					log.Debug("Detected file ", infoName)
					detected := newFile{
						Filename:  filepath.Join(folder, infoName),
						EventType: etype,
					}
					select {
					case <-done:
						return
					case filehit <- detected:
						break
					}
				}
				visited[infoName] = true
			}
		}
		for k, v := range visited {
			if v == false {
				log.Debug("Detected removal of file ", k)
				detected := newFile{
					Filename:  filepath.Join(folder, k),
					EventType: FILE_DELETED,
				}
				select {
				case <-done:
					return
				case filehit <- detected:
					delete(visited, k)
				}
			}
		}
	}
	go func() {
		// First scan: seek to end.
		scan(FILE_EXISTING)
		// Subsequent scans: seek to start
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				scan(FILE_CREATED)
			}
		}
	}()
	return filehit
}

// Keep the given line?
func (c *collector) Keep(lineStr string) bool {
	if c.Skip != nil {
		for _, skip := range c.Skip {
			if strings.Contains(lineStr, skip) {
				return false
			}
		}
	}
	return true
}

func Monitor(ctx context.Context, folder string, buffer int, skip []string, logStart string) {

	c := collector{
		Lines:    make(chan string, buffer),
		LogStart: logStart,
		Skip:     skip,
	}
	go c.output()

	done := make(chan struct{})
	scan := scanFolder(done, folder)
	wait := sync.WaitGroup{}
	for {
		select {
		case <-ctx.Done():
			// Tell all inputs to stop, wait for them
			close(done)
			wait.Wait()
			// After all inputs are done, close Lines
			close(c.Lines)
			return
		case newFile := <-scan:
			// Ignore FILE_DELETED for now
			if newFile.EventType == FILE_DELETED {
				log.Error("Unexpected removal of file ", newFile.Filename)
				break
			}
			seekEnd := false
			if newFile.EventType == FILE_EXISTING {
				seekEnd = true
			}
			wait.Add(1)
			go func() {
				defer wait.Done()
				c.input(done, newFile.Filename, seekEnd)
			}()
		}
	}
}

/// Output reads lines from channel and writes them to stdout
func (c *collector) output() {
	defer log.Debug("Exit output goroutine")
	log.Debug("Entering output goroutine")
	for line := range c.Lines {
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

/// Input read lines from file and writes them to channel
func (c *collector) input(done chan struct{}, filename string, seek_end bool) {
	defer log.Debug("Exit input goroutine for file ", filename)
	log.Debug("Entering input goroutine for file ", filename)
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
		panic(err)
	}
	// OJO!!!! Solo llamar a t.Cleanup si no se va a volver a reabrir
	// el fichero. En otro caso, mejor llamar a t.Stop.
	defer t.Cleanup()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	buffer := make([]string, 0, MAXBUFFER)
	flush := func() {
		if len(buffer) > 0 {
			if c.Keep(buffer[0]) {
				c.Lines <- strings.Join(buffer, " ")
			}
		}
		buffer = buffer[:0]
	}
	defer flush()

	for {
		select {
		case <-done:
			return
		case line := <-t.Lines:
			if line == nil {
				return
			}
			if line.Err != nil {
				break
			}
			lineStr := line.Text
			if lineStr == "" {
				break
			}
			switch {
			case c.LogStart == "":
				// No prefix to merge, just flush line
				if c.Keep(lineStr) {
					c.Lines <- lineStr
				}
				break
			case strings.HasPrefix(lineStr, c.LogStart) || len(buffer) >= MAXBUFFER:
				flush()
				fallthrough
			default:
				buffer = append(buffer, lineStr)
			}
		case <-ticker.C:
			flush()
		}
	}
}

func main() {

	parser := argparse.NewParser("tailall", "Tails several files in a folder")
	// Create folder flag
	folder := parser.String("f", "folder", &argparse.Options{Required: true, Help: "Path to log folder"})
	logStart := parser.String("l", "logstart", &argparse.Options{Required: false, Help: "Merge lines starting with this prefix"})
	debug := parser.Flag("d", "debug", &argparse.Options{Required: false, Help: "Enabled debug trace"})
	skip := parser.StringList("s", "skip", &argparse.Options{Required: false, Help: "Skip lines containing this text"})
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

	// Wait for signal
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Debug("Received interrupt signal, exiting")
		cancel()
	}()

	// Start monitor
	logstartParam := ""
	if logStart != nil {
		logstartParam = *logStart
	}
	skipParam := []string{}
	if skip != nil {
		skipParam = *skip
	}
	Monitor(ctx, *folder, MAXBUFFER, skipParam, logstartParam)
}
