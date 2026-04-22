# Works with both the legacy builder and BuildKit. Enable BuildKit
# (DOCKER_BUILDKIT=1 or `docker buildx build`) for faster rebuilds via
# automatic layer cache; no Dockerfile changes needed.

# ---- build stage ----
FROM golang:1.25.9-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
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
    && chown -R streamer:streamer /data /out \
    && chmod -R 0777 /data /out

COPY --from=builder /out/open-streamer /usr/local/bin/open-streamer

USER streamer
WORKDIR /data

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/open-streamer"]
