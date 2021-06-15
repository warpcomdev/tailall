# tailall

This app tails all files in a given folder. This is intended for tailing postgresql logs from a sidecar container.

## Usage

`tailall -f <folder> -l <logstart>`

Tails all files in the given folder.

If `-l <logstart>` is provided, merges consecutive lines that do not begin by the `logstart` prefix. This is intended to concatenate log lines when postgresql query log is enabled, and there are multiline queries.

## Docker

This application is provided as a docker container. It can be run like this, for example:

```bash
docker run --rm -it -v /path/to/log/folder:/logs:ro warpcomdev/tailall:latest -f /logs
```
