FROM golang:1.16 AS builder

WORKDIR /app
COPY go.mod fom.sum main.go /app/
RUN  CGO_ENABLED=0 go build

FROM scratch
COPY --from=builder /app/tailall /tailall
ENTRYPOINT ["/tailall"]
