# tailall

This app tails all files in a given folder. This is intended for tailing postgresql logs from a sidecar container.

## Usage

`tailall -f <folder> -l <logstart> -s <skip>`

Tails all files in the given folder.

If `-l <logstart>` is provided, merges consecutive lines that do not begin by the `logstart` prefix. This is intended to concatenate log lines when postgresql query log is enabled, and there are multiline queries.

If `-s <skip>` is provided, lines containing the `skip` text are skipped. `-s <skip>` can be provided several times in the command line.

## Docker

This application is provided as a docker container. It can be run like this, for example:

```bash
docker run --rm -it -v /path/to/log/folder:/logs:ro warpcomdev/tailall:latest -f /logs
```

## Caveats

- Currently does not support removal of log files. If a log file is removed, it won't be tailed when created again.
- New log files are scanned every 10 seconds. So up to 10 seconds of log can accumulate in a new log file before tailall catches up.
