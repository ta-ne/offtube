FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY . .
RUN go build -ldflags="-s -w" -o offtube .

FROM alpine:3
RUN apk add --no-cache \
      ffmpeg \
      yt-dlp \
      deno \
      ca-certificates

COPY --from=builder /src/offtube /usr/local/bin/offtube

RUN mkdir -p /data
VOLUME /data
WORKDIR /

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/offtube"]
