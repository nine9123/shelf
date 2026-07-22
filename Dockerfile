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
# bsdtar (libarchive) reads the non-zip formats (rar, 7z, tar family); zip/cbz is
# native. gosu lets the entrypoint drop from root to a configurable PUID/PGID, so
# a bind-mounted cache works regardless of who owns it on the host.
RUN apt-get update \
 && apt-get install -y --no-install-recommends libarchive-tools gosu \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /data/library /data/cache
COPY --from=build /out/shelf /usr/local/bin/shelf
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
ENV ROOT_DIR=/data/library \
    CACHE_DIR=/data/cache \
    LISTEN_ADDR=:8080 \
    PUID=1000 \
    PGID=1000
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
