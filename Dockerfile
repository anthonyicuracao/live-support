# Build a static live-support binary (pure-Go SQLite, CGO off) and ship it on a
# tiny Alpine image with CA certs (Cloudflare TURN API) + timezone data. Runs as
# a non-root user; /data is a volume (named volumes inherit its ownership). Binds
# all interfaces so the reverse proxy container can reach it on the compose net.
FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/live-support .

FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 lsupport \
 && mkdir -p /data && chown lsupport:lsupport /data
COPY --from=build /out/live-support /usr/local/bin/live-support
USER lsupport
WORKDIR /data
ENV DATA_DIR=/data BIND_ADDR=0.0.0.0 PORT=8000
VOLUME ["/data"]
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/live-support"]
