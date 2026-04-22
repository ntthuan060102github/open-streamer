# syntax=docker/dockerfile:1.7

# ---- build stage ----
FROM golang:1.25.9-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/open-streamer \
        ./cmd/server

# ---- runtime stage ----
FROM alpine:3.21

RUN apk add --no-cache ffmpeg ca-certificates tzdata wget tini \
    && addgroup -S -g 10001 streamer \
    && adduser  -S -u 10001 -G streamer streamer \
    && mkdir -p /data /out/hls /out/dash /out/dvr \
    && chown -R streamer:streamer /data /out

COPY --from=builder /out/open-streamer /usr/local/bin/open-streamer

USER streamer
WORKDIR /data

# HTTP API + HLS/DASH / gRPC / Prometheus / RTMP / RTSP / SRT (UDP)
EXPOSE 8080 9090 9091 1935 8554 9999/udp

VOLUME ["/data", "/out/hls", "/out/dash", "/out/dvr"]

HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/open-streamer"]
