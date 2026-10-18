package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// App holds the shared state: config, the two databases, the work channels, and
// scan status. The zero-value channels are set up in main.
type App struct {
	cfg      Config
	index    *sql.DB
	user     *sql.DB
	jobs     chan job
	results  chan result
	logins   *loginLimiter
	scanning atomic.Bool
	lastScan atomic.Int64
	sem      *Semantic

	// ctx is cancelled when SIGTERM/SIGINT arrives. Scans and the semantic
	// worker watch it so they stop handing out work and the process can exit.
	ctx context.Context
}

// shutdownBudget bounds the whole clean stop. Orchestrators such as idlegate
// run `docker stop -t 10`, so finish well inside that and never get SIGKILLed.
const shutdownBudget = 4 * time.Second

// context returns the app's lifetime context (Background when unset, as in
// tests that build an App by hand).
func (a *App) context() context.Context {
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}

func main() {
	// Handle SIGTERM ourselves: as a container's PID 1 the default disposition
	// does not end the process cleanly, and `docker stop` would report a
	// non-zero exit code (which TrueNAS shows as a crashed app).
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, os.Interrupt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := LoadConfig()
	if err := os.MkdirAll(cfg.CoversDir, 0o755); err != nil {
		log.Fatalf("cannot create cache dir: %v", err)
	}
	index, err := openIndexDB(cfg.IndexDBPath)
	if err != nil {
		log.Fatalf("index db: %v", err)
	}
	user, err := openUserDB(cfg.UserDBPath)
	if err != nil {
		log.Fatalf("user db: %v", err)
	}

	app := &App{
		cfg:     cfg,
		index:   index,
		user:    user,
		jobs:    make(chan job, cfg.Workers*2),
		results: make(chan result, cfg.Workers*2),
		logins:  newLoginLimiter(),
		ctx:     ctx,
	}
	go sweepTempCovers(cfg.CoversDir)
	app.startWorkers()
	go app.scanLoop()

	// Optional semantic search: only active when OLLAMA_URL is set.
	app.sem = &Semantic{cfg: cfg, index: index, user: user, ctx: ctx}
	if cfg.OllamaURL != "" {
		app.sem.ol = NewOllama(cfg.OllamaURL, cfg.EmbedModel, cfg.VisionModel)
		app.sem.LoadIndex()
		go app.sem.Run()
		log.Printf("semantic: enabled via %s (embed=%s vision=%s), %d embeddings loaded",
			cfg.OllamaURL, cfg.EmbedModel, cfg.VisionModel, app.sem.count())
	}

	switch {
	case cfg.APIKey == "":
		log.Printf("auth: DISABLED — anyone who can reach %s can read the library (set API_KEY to require a key)", cfg.ListenAddr)
	case len(cfg.APIKey) < 16:
		log.Printf("auth: enabled, but API_KEY is only %d characters — use something long and random", len(cfg.APIKey))
	default:
		log.Printf("auth: enabled (API_KEY)")
	}

	log.Printf("shelf: library=%s cache=%s workers=%d listening on %s",
		cfg.RootDir, cfg.CacheDir, cfg.Workers, cfg.ListenAddr)
	// The port opens only now, after the databases are open and migrated, so
	// the first connection that succeeds can be served (see /healthz).
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: app.routes()}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		log.Fatal(err) // a real failure (e.g. port in use): exit non-zero
	case sig := <-sigc:
		log.Printf("shelf: received %s, shutting down", sig)
	}

	// Stop background work first so it unwinds while HTTP connections drain.
	// Unfinished archives are simply not marked indexed and get redone by the
	// next scan; nothing half-done is ever committed.
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer scancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close() // long-lived reads (big page downloads) don't get to hold us up
	}
	app.waitScanStopped(sctx)
	index.Close()
	user.Close()
	log.Printf("shelf: shutdown complete")
}

// waitScanStopped waits until no scan is writing to the index, or ctx expires.
// After cancellation a scan stops at its next result, so this is quick.
func (a *App) waitScanStopped(ctx context.Context) {
	for a.scanning.Load() {
		select {
		case <-ctx.Done():
			log.Printf("shelf: scan still stopping at shutdown deadline")
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// sweepTempCovers removes temp files left by a cover write that was killed
// mid-way (SIGKILL, power loss). Real covers are only ever renamed into place.
func sweepTempCovers(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("covers: remove %s: %v", m, err)
		}
	}
}
