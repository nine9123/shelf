package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config is loaded from the environment. The only two paths that matter are
// RootDir (the source of truth, mounted read-only) and CacheDir (everything
// regenerable, plus the small user database).
type Config struct {
	RootDir      string
	CacheDir     string
	CoversDir    string
	IndexDBPath  string
	UserDBPath   string
	ListenAddr   string
	ScanInterval time.Duration
	Workers      int
	ThumbWidth   int

	// Shared secret for the whole app. Empty means no authentication at all.
	APIKey string

	// Semantic search (optional). Empty OllamaURL disables the whole feature.
	OllamaURL     string
	EmbedModel    string
	VisionModel   string
	SemanticDelay time.Duration // pause between archives so the GPU job stays low-priority
}

func LoadConfig() Config {
	c := Config{
		RootDir:      envStr("ROOT_DIR", "/data/library"),
		CacheDir:     envStr("CACHE_DIR", "/data/cache"),
		ListenAddr:   envStr("LISTEN_ADDR", ":8080"),
		ScanInterval: envDur("SCAN_INTERVAL", 15*time.Minute),
		Workers:      envInt("WORKER_CONCURRENCY", runtime.NumCPU()),
		ThumbWidth:   envInt("THUMB_WIDTH", 400),
		// Trimmed because keys are usually pasted, and a trailing newline from
		// `API_KEY=$(cat key.txt)` would otherwise never match what's typed in.
		APIKey: strings.TrimSpace(envStr("API_KEY", "")),

		OllamaURL:     envStr("OLLAMA_URL", ""),
		EmbedModel:    envStr("EMBED_MODEL", "bge-m3"),
		VisionModel:   envStr("VISION_MODEL", "llava"),
		SemanticDelay: envDur("SEMANTIC_DELAY", 0),
	}
	if c.Workers < 1 {
		c.Workers = 1
	}
	c.CoversDir = filepath.Join(c.CacheDir, "covers")
	c.IndexDBPath = envStr("INDEX_DB_PATH", filepath.Join(c.CacheDir, "index.db"))
	c.UserDBPath = envStr("USER_DB_PATH", filepath.Join(c.CacheDir, "user.db"))
	return c
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
