# syntax=docker/dockerfile:1

# Build natively per target arch. The WebRTC AAC->Opus transcoder uses cgo
# (go-astiav -> ffmpeg), and cgo can't easily cross-compile the ffmpeg C libs,
# so we drop BUILDPLATFORM cross-compilation and let buildx build each arch
# natively (emulated). apk then installs the matching-arch ffmpeg headers/libs.
FROM golang:1.26-alpine AS builder

# git for `go get`; gcc/musl-dev/pkgconf + ffmpeg-dev for the cgo transcoder.
RUN apk add --no-cache git gcc musl-dev pkgconf ffmpeg-dev

WORKDIR /go/src/app
COPY . .

ENV CGO_ENABLED=1
RUN go get \
    && go mod download \
    && go build -o rtsp-to-web

FROM alpine:3.23

WORKDIR /app

# Runtime shared libraries for the cgo transcoder
# (libavcodec/avutil/swresample + libopus, pulled in by ffmpeg-libs).
RUN apk add --no-cache ffmpeg-libs

COPY --from=builder /go/src/app/rtsp-to-web /app/
COPY --from=builder /go/src/app/web /app/web

RUN mkdir -p /config
COPY --from=builder /go/src/app/config.json /config

ENV GO111MODULE="on"
ENV GIN_MODE="release"

CMD ["./rtsp-to-web", "--config=/config/config.json"]
