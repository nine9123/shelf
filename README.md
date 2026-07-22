# Shelf

A small self-hosted reader for a library of sequential-image archives (`.zip` /
`.cbz` — folders of images that, in order, tell a story). Point it at a folder,
open a browser, read. It indexes in the background, makes cover thumbnails, and
lets you search, favorite, and resume where you left off.

This is an intentionally minimal, end-to-end slice: enough of every part
(scanner → index → API → reader) to actually use, and to see which features are
worth building out next. It is a single ~7 MB Go binary with one dependency.

## Core idea

There are three kinds of data, and they are kept strictly separate:

| Data | Where | Authoritative? |
|------|-------|----------------|
| Your archives | `ROOT_DIR` (mounted **read-only**) | **Yes** — the source of truth |
| Index + cover thumbnails | `CACHE_DIR/index.db`, `CACHE_DIR/covers/` | No — a disposable cache |
| Favorites + read progress | `CACHE_DIR/user.db` | **Yes** — the only durable app state |

The library is never written to. The index is a cache: delete `index.db` and it
rebuilds itself on the next scan. The one thing that isn't derivable from your
files — what you've favorited and how far you've read — lives in a separate
`user.db`, keyed by the archive's path so it **survives a full cache rebuild**.

## Quick start

### Docker (recommended)

```bash
cp .env.example .env
# edit .env and set LIBRARY_PATH to your archive folder
docker compose up --build
```

Open http://localhost:8080. The first scan starts automatically; covers appear
as they're generated. Add files later and press **Scan** (or wait for the
interval).

### Local (Go 1.22+)

```bash
ROOT_DIR=/path/to/archives CACHE_DIR=/tmp/shelf-cache go run .
# open http://localhost:8080
```

## How it works

1. **Scan** walks `ROOT_DIR` and diffs it against the index by `(path, size,
   mtime)`. Only new or changed archives are queued; vanished files are pruned.
2. A **worker pool** opens each archive, lists its image entries in natural
   order (`page2` before `page10`), reads the first `.txt` as metadata, and
   writes a cover thumbnail. Workers never touch the database — the scanner is
   the sole writer, so there's no write contention.
3. The **API** serves metadata and covers from the cache. Page images are
   streamed **directly out of the ZIP on demand** — pages are never extracted to
   disk, so a 2 TB library needs almost no cache space beyond thumbnails.
4. Scans run at startup, every `SCAN_INTERVAL`, and on `POST /api/scan`. There's
   no file-watcher on purpose: over SMB/NAS mounts filesystem events aren't
   reliable, so polling + a manual trigger is the robust choice.

## API

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/api/archives?q=&fav=1&limit=&offset=` | List / search |
| GET | `/api/archives/{id}` | Detail (metadata, favorite, `lastPage`) |
| GET | `/api/archives/{id}/cover` | Cover thumbnail (JPEG) |
| GET | `/api/archives/{id}/page/{n}` | Stream page image `n` (ETag-cached) |
| PUT / DELETE | `/api/archives/{id}/favorite` | Toggle favorite |
| PUT | `/api/archives/{id}/progress` | Save `{ "page": n }` |
| GET | `/api/favorites` | Favorited archives |
| POST | `/api/scan` | Trigger a scan |
| GET | `/api/status` | Counts, scanning flag, last scan time |

`id` is the cache row id (fine within a session). Favorites/progress are stored
by **path**, so they're stable across rebuilds even though ids are reassigned.

## Configuration

All via environment variables:

| Var | Default | Meaning |
|-----|---------|---------|
| `ROOT_DIR` | `/data/library` | Archive root (read-only) |
| `CACHE_DIR` | `/data/cache` | Index, covers, and `user.db` |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `SCAN_INTERVAL` | `15m` | Re-scan period; `0` = once at boot only |
| `WORKER_CONCURRENCY` | number of CPUs | Parallel indexers |
| `THUMB_WIDTH` | `400` | Cover width in px |
| `INDEX_DB_PATH` / `USER_DB_PATH` | under `CACHE_DIR` | Override DB locations |

## Resetting the cache

```bash
# drop the index + thumbnails; favorites and progress are untouched
rm -f  "$CACHE_DIR"/index.db*
rm -rf "$CACHE_DIR"/covers
# next scan rebuilds them
```

To also wipe favorites/progress, delete `user.db*` as well.

## Extending it

`user.db` is where durable, non-derivable features belong. Adding one follows
the favorites pattern: a table keyed by `archive_path`, a couple of handlers,
resolve the request's `id` to a path with `a.lookup(id)`, then read/write
`user.db`. Read progress is already wired this way as a second example — use it
as a template for collections, tags, ratings, bookmarks, etc.

## Documented upgrade paths

The slice deliberately favors a clean build over maximal features. Each of these
is a small, isolated change when you want it:

- **Full-text search (FTS5).** Search currently uses SQL `LIKE`, which is fine
  for a few hundred thousand rows. For ranked, tokenized search, add an FTS5
  virtual table and query it with `MATCH`:

  ```sql
  CREATE VIRTUAL TABLE archives_fts USING fts5(title, meta, path, content='');
  -- keep it in sync in writeResult(); search: SELECT rowid FROM archives_fts WHERE archives_fts MATCH ?
  ```

  With this driver, rebuild with the FTS tag: `go build -tags "sqlite_fts5" .`
  (The pure-Go driver below ships FTS5 with no tag.)

- **Faster thumbnails + WebP (libvips).** The built-in resizer is pure Go and
  single-threaded per image — fine to start, slow across a full 2 TB first pass.
  Swap `makeThumb` for [govips](https://github.com/davidbyttow/govips) (libvips
  bindings) to get an order-of-magnitude speedup and WebP output. This adds a
  libvips system dependency to the build stage.

- **cgo-free build (modernc.org/sqlite).** To drop cgo entirely (simpler static
  binary, FTS5 included), replace the driver import in `db.go`:

  ```go
  _ "modernc.org/sqlite"           // instead of github.com/mattn/go-sqlite3
  ```

  Change the driver name from `sqlite3` to `sqlite`, and the DSN pragma syntax to
  modernc's form, e.g.
  `file:PATH?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)`. Then set
  `CGO_ENABLED=0` and the runtime image can be `scratch`/`distroless`.

- **Content hashing.** Change detection uses `(size, mtime)`, which is fast and
  correct for normal use. If you rewrite archives in place without changing size,
  add an optional content hash to the fingerprint.

## Notes & limitations

- Cover generation decodes JPEG/PNG/GIF (standard library). WebP/AVIF covers
  aren't decoded in this slice, so those archives index fine but show no cover
  until you adopt the libvips path above.
- `.cbz` is just a renamed `.zip`; both are handled. Other container formats
  (`.cbr`/RAR, `.7z`) are not.
- Page order is natural-sorted by filename; it assumes filenames sort into
  reading order, which is the usual convention for these archives.
