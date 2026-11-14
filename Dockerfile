FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/vbackuper ./cmd/vbackuper

FROM alpine:3.21
ARG PG_VERSION=17
RUN apk add --no-cache tzdata ca-certificates zstd \
      postgresql${PG_VERSION}-client mariadb-client \
 && addgroup -g 10001 vbackuper \
 && adduser -D -u 10001 -G vbackuper -h /home/vbackuper vbackuper \
 && mkdir -p /data /backups /sources \
 && chown -R vbackuper:vbackuper /data /backups

COPY --from=build /out/vbackuper /usr/local/bin/vbackuper

USER vbackuper
EXPOSE 8787
VOLUME ["/data", "/backups"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8787/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/vbackuper"]
