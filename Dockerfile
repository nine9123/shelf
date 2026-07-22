# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO is on because the SQLite driver (mattn/go-sqlite3) compiles the bundled
# SQLite C amalgamation directly into the binary. No system libsqlite needed.
ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /out/shelf .

# ---- runtime ----
FROM debian:bookworm-slim
RUN useradd -r -u 10001 shelf \
 && mkdir -p /data/library /data/cache \
 && chown -R shelf /data
COPY --from=build /out/shelf /usr/local/bin/shelf
USER shelf
ENV ROOT_DIR=/data/library \
    CACHE_DIR=/data/cache \
    LISTEN_ADDR=:8080
EXPOSE 8080
# Only the cache is a volume. The library is bind-mounted read-only at run time.
VOLUME ["/data/cache"]
ENTRYPOINT ["/usr/local/bin/shelf"]
