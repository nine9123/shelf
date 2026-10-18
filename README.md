# Shelf

A small self-hosted reader for a library of sequential-image archives — folders
of images that, in order, tell a story. It reads `.zip`/`.cbz`, `.rar`/`.cbr`,
`.7z`/`.cb7`, and the tar family (`.tar`, `.tar.gz`, `.tar.xz`, `.tar.bz2`).
Point it at a folder, open a browser, read. It indexes in the background, makes
cover thumbnails, and lets you search, favorite, and resume where you left off.

This is an intentionally minimal, end-to-end slice: enough of every part
(scanner → index → API → reader) to actually use, and to see which features are
worth building out next. It is a single small Go binary. Zip needs nothing else;
the other formats are read through `bsdtar` (libarchive), which the Docker image
installs for you.

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
# edit .env: set LIBRARY_PATH and CACHE_PATH, and PUID/PGID to match your files
docker compose up --build
```

Open http://localhost:8080. The first scan starts automatically; covers appear
as they're generated. Add files later and press **Scan** (or wait for the
interval).

#### `LIBRARY_PATH` vs `ROOT_DIR`

They name the same thing — "where my archives live" — at two layers:

- `ROOT_DIR` is the only path the *program* knows. It's where the app looks.
- Under Docker the app runs in a container with its own filesystem, so your real
  files aren't visible inside it. Compose bind-mounts your host folder into the
  container, where `ROOT_DIR` is fixed at `/data/library`. **`LIBRARY_PATH` is
  just the host folder to mount.** You set `LIBRARY_PATH`; you never touch
  `ROOT_DIR`.
- Running locally (below) there's no container, so you point `ROOT_DIR` straight
  at your real folder and `LIBRARY_PATH` is irrelevant.

The cache works the exact same way: **`CACHE_PATH`** is the host folder, and
`CACHE_DIR` (`/data/cache`) is where the app writes inside the container. You set
`CACHE_PATH`; you never touch `CACHE_DIR`.

You can skip the `.env` file and pass values inline:
`LIBRARY_PATH=/data/comics CACHE_PATH=/data/shelf-cache docker compose up --build`.

#### Where the cache lives

`CACHE_PATH` is a host folder you choose (see `.env`); Shelf keeps `index.db`,
`covers/`, and `user.db` there. It's created if missing and defaults to `./cache`
next to the compose file.

Because the server runs unprivileged, the container starts as root only long
enough to take ownership of that folder, then drops to **`PUID:PGID`** before
doing anything else. Set those to the account that owns your archives (`id -u` /
`id -g`) so both library reads and cache writes work with no manual `chown`. The
library stays read-only and is never modified.

> Moving the cache to a new `CACHE_PATH` (e.g. from an earlier named volume)
> starts a fresh index — that's fine, it's regenerable. To carry over favorites
> and reading progress, copy the old `user.db` into the new folder first.

### Local (Go 1.22+)

For the non-zip formats you need `bsdtar` on your PATH
(`apt install libarchive-tools`, or `brew install libarchive`). Then:

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
   streamed **straight out of the archive on demand** — zip natively (random
   access), other formats via libarchive. Pages are never extracted to disk, so
   a 2 TB library needs almost no cache space beyond thumbnails.
4. Scans run at startup, every `SCAN_INTERVAL`, and on `POST /api/scan`. There's
   no file-watcher on purpose: over SMB/NAS mounts filesystem events aren't
   reliable, so polling + a manual trigger is the robust choice.

## API

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/api/archives?q=&fav=1&hideDupes=1&limit=&offset=` | List / search |
| GET | `/api/archives/{id}` | Detail (metadata, category, grouped tags, favorite, `lastPage`) |
| GET | `/api/archives/{id}/cover` | Cover thumbnail (JPEG) |
| GET | `/api/archives/{id}/page/{n}` | Stream page image `n` (ETag-cached) |
| GET | `/api/archives/{id}/thumb/{n}` | Small JPEG thumbnail of page `n` (for the detail grid) |
| PUT / DELETE | `/api/archives/{id}/favorite` | Toggle favorite |
| PUT | `/api/archives/{id}/progress` | Save `{ "page": n }` |
| GET | `/api/favorites` | Favorited archives |
| POST | `/api/scan` | Trigger a scan |
| GET | `/api/status` | Counts, scanning flag, last scan time |
| GET | `/api/errors` | Archives that failed to index, with the reason |
| GET | `/api/id-report` | Duplicate IDs (with size + page count) and files missing an ID |
| GET | `/api/namespaces` | Distinct tag namespaces present, with counts |
| GET | `/api/auth/status` | `{required, authenticated}` — public |
| POST | `/api/auth/login` | Exchange `{ "key": "…" }` for a session cookie — public |
| POST | `/api/auth/logout` | Clear the session cookie — public |
| GET | `/healthz` | Readiness check, `200` once the app can serve — public |

`id` is the cache row id (fine within a session). Favorites/progress are stored
by **identity** (the leading `#id`), so they follow a file across moves, renames,
and re-encoding, and survive rebuilding the cache.

### Search syntax (the `q` parameter)

- Plain words match the title, path, and raw info text (AND across words).
- `namespace:value` filters by a tag — e.g. `artist:foo`, `parody:bar`. Tags are
  always namespaced so there's no ambiguity about what you're matching.
- Quote values containing spaces: `artist:"john doe"`.
- `category:name` filters the category.
- Everything combines with **AND**: `parody:touhou artist:"john doe"`.

### Metadata & tags

If an archive contains an `info.txt`, the scanner parses its `Category:` line and
its `> namespace: v1, v2` tag lines into a searchable tag index (`tags` table) and
a `category` column — both derived, both in the disposable cache, rebuilt on scan.
Length isn't parsed; the archive's image count is the length.

## Authentication (optional)

Set `API_KEY` and the whole app goes behind a single shared key. Leave it unset
and Shelf is open to anyone who can reach the port, exactly as before — so an
existing deployment doesn't change behaviour until you opt in. The startup log
says which mode you're in.

```bash
# generate something long and random, then put it in .env
openssl rand -base64 32
```

You get a login page: type the key once, and it's kept in the browser's
localStorage so return visits go straight through. **Lock** in the header
forgets it on that device.

**How it works.** The browser trades the key for an `HttpOnly` session cookie at
`/api/auth/login`. The cookie — not a header — is what authenticates requests,
because the reader loads pages and covers as `<img src="/api/…">` and an image
request can't carry an `Authorization` header. The saved key exists so a
returning visitor can re-establish the cookie silently instead of retyping it;
if the cookie expires mid-session the page repairs it and retries without a
prompt. Scripts can skip all of that and send the key directly:

```bash
curl -H "X-API-Key: $API_KEY" http://localhost:8080/api/archives
curl -H "Authorization: Bearer $API_KEY" http://localhost:8080/api/status
```

Only the HTML shell (`/`) and the three `/api/auth/*` endpoints are reachable
without a key; everything else — including every cover and page — returns 401.
Failed logins are throttled per client IP (8 per 5 minutes).

**What this is and isn't.** It's one shared key for one household, not user
accounts: there's no per-user state, and anyone with the key has full access
including **Scan** and cover edits. The key crosses the wire in the clear over
plain HTTP, so if you expose Shelf beyond your LAN, put it behind a reverse
proxy with TLS — the cookie sets its `Secure` flag automatically when the
connection is HTTPS (or when the proxy sends `X-Forwarded-Proto: https`).

## Semantic search (optional)

Natural-language, "by meaning" search over your library — e.g. *"cosmic felines
in a bright spaceship"* — powered by a local [Ollama](https://ollama.com) server.
Disabled unless `OLLAMA_URL` is set.

How it works: a low-priority background worker captions each archive's cover with
a vision model, combines that with the title/tags/category, and embeds the result
with a multilingual model. Queries are embedded the same way and ranked by cosine
similarity in memory (no vector database — ~118 MB for 38k archives, sub-ms). Toggle
**Semantic** in the header and type a description.

Setup:

```
ollama pull bge-m3     # multilingual embeddings — handles Japanese + English
ollama pull llava      # vision model for captions (or 'moondream' for ~2-3x speed)
```

Then set `OLLAMA_URL` (see `.env.example`). Notes and trade-offs:

- **It's a background batch.** Captioning is ~1 vision inference per archive; on a
  single mid-range GPU, tens of thousands of archives take **hours to a couple of
  days**. Search works incrementally over whatever's embedded so far (favorites
  first); check progress at `GET /api/semantic/status`.
- **This is situational search, not plot search.** A cover caption captures setting,
  characters, and mood — great for "find the space-station ones" — but not specific
  story beats. True plot search would need OCR of the interior pages (a much larger,
  language-dependent job); this is a natural future phase.
- **Embeddings are durable.** They live in `user.db` keyed by identity, so dropping
  the disposable cache never forces re-inference, and captions follow a file through
  moves, renames, and re-encodes.
- If Ollama is unreachable the app runs normally; the worker retries and semantic
  queries return a clear error.

New endpoints: `GET /api/semantic?q=` (ranked results) and `GET /api/semantic/status`.

## Configuration

The app itself reads these:

| Var | Default | Meaning |
|-----|---------|---------|
| `ROOT_DIR` | `/data/library` | Archive root (read-only) |
| `CACHE_DIR` | `/data/cache` | Index, covers, and `user.db` |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `SCAN_INTERVAL` | `15m` | Re-scan period; `0` = once at boot only |
| `WORKER_CONCURRENCY` | number of CPUs | Parallel indexers |
| `THUMB_WIDTH` | `400` | Cover width in px |
| `API_KEY` | — (empty) | Shared key for the whole app; empty = no authentication |
| `INDEX_DB_PATH` / `USER_DB_PATH` | under `CACHE_DIR` | Override DB locations |

These are Docker-only (used by Compose and the entrypoint, not the binary):

| Var | Default | Meaning |
|-----|---------|---------|
| `LIBRARY_PATH` | — (required) | Host archive folder → mounted at `ROOT_DIR` |
| `CACHE_PATH` | `./cache` | Host cache folder → mounted at `CACHE_DIR` |
| `PUID` / `PGID` | `1000` | User/group the server runs as |

## Running behind idlegate (start/stop on demand)

Shelf can be stopped while idle and started on the next request:

- **Clean stop.** On SIGTERM (or Ctrl-C) it stops scanning, drains HTTP
  connections, closes the databases, and exits **0** within about 4 s, inside
  `docker stop`'s 10 s grace. The logs show `received terminated, shutting down`
  and `shutdown complete`.
- **Readiness.** The port opens only after the databases are open, and
  `GET /healthz` needs no key, so point the route at it:
  `health=/healthz` (not `/api/status`, which answers 401 when `API_KEY` is set).
- **Interrupted scans resume.** Archives not yet written to the index stay
  un-indexed and are redone by the next scan, which runs at every start (interval
  scans don't fire while stopped). Covers are written to a temp file and renamed,
  so even a SIGKILL never leaves a truncated cover.
- **Ollama down.** The semantic worker retries with backoff (1 s … 30 s) and
  never stores a caption-less description because Ollama was unreachable.

A first full index of a large library won't finish if it is stopped every idle
period; stop the gate (`docker stop idlegate`) until that first scan is done.

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

- **Per-archive page cache.** For the tar family and solid rar/7z (no random
  access), serving page *n* re-streams the archive to that entry. Since the
  reader moves in order and preloads the next page, a small in-memory LRU of
  decoded page bytes — keyed by `(path, mtime, n)` in `handlePage` — makes a
  read-through effectively linear. Zip/7z with real per-entry access don't need
  it.

## Notes & limitations

- Cover generation decodes JPEG/PNG/GIF (standard library). WebP/AVIF covers
  aren't decoded in this slice, so those archives index fine but show no cover
  until you adopt the libvips path above.
- **Formats.** Zip/cbz is read natively. RAR, 7z, and the tar family are read
  through libarchive (`bsdtar`), read-only — including RAR5. Creating or writing
  these formats is out of scope; Shelf never modifies your files.
- **Sequential formats are slower per page.** Zip and (mostly) 7z allow
  per-entry access; the tar family and *solid* rar/7z archives have none, so a
  page is produced by streaming the archive up to that entry. That's fine for
  the typical 5–50 MB story here, but jumping around a very large solid archive
  will feel it. The per-archive page cache below removes this.
- Page order is natural-sorted by filename; it assumes filenames sort into
  reading order, which is the usual convention for these archives.
- **Auth is one shared key, not user accounts.** `API_KEY` gates everything or
  nothing; there are no roles, no per-user favorites, and no way to revoke one
  device without rotating the key for all of them. That matches the app's single
  `user.db` — real accounts would mean keying favorites and progress by user,
  which is a much larger change. Left off by default so existing setups keep
  working; see [Authentication](#authentication-optional).
