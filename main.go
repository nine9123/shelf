package main

import (
	"database/sql"
	"embed"
	"log"
	"net/http"
	"os"
	"sync/atomic"
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
	scanning atomic.Bool
	lastScan atomic.Int64
	sem      *Semantic
}

func main() {
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
	}
	app.startWorkers()
	go app.scanLoop()

	// Optional semantic search: only active when OLLAMA_URL is set.
	app.sem = &Semantic{cfg: cfg, index: index, user: user}
	if cfg.OllamaURL != "" {
		app.sem.ol = NewOllama(cfg.OllamaURL, cfg.EmbedModel, cfg.VisionModel)
		app.sem.LoadIndex()
		go app.sem.Run()
		log.Printf("semantic: enabled via %s (embed=%s vision=%s), %d embeddings loaded",
			cfg.OllamaURL, cfg.EmbedModel, cfg.VisionModel, app.sem.count())
	}

	log.Printf("shelf: library=%s cache=%s workers=%d listening on %s",
		cfg.RootDir, cfg.CacheDir, cfg.Workers, cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, app.routes()); err != nil {
		log.Fatal(err)
	}
}
