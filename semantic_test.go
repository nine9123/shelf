package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeVec(t *testing.T) {
	in := []float32{0, 1.5, -2.25, 3.0e9, 0.0001}
	out := decodeVec(encodeVec(in))
	if len(out) != len(in) {
		t.Fatalf("len %d != %d", len(out), len(in))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("roundtrip[%d]: %v != %v", i, out[i], in[i])
		}
	}
}

func TestNormalizeAndDot(t *testing.T) {
	n := normalizeVec([]float32{3, 4}) // -> (0.6, 0.8), length 1
	if d := dotVec(n, n); math.Abs(float64(d)-1) > 1e-5 {
		t.Fatalf("unit vector self-dot = %v, want ~1", d)
	}
}

func TestSearchVecRanking(t *testing.T) {
	s := &Semantic{pos: map[string]int{}}
	s.addVec("a", []float32{1, 0, 0})
	s.addVec("b", []float32{0, 1, 0})
	s.addVec("c", []float32{0.9, 0.1, 0}) // closest to the query below

	hits := s.searchVec([]float32{1, 0, 0}, 3)
	if len(hits) != 3 {
		t.Fatalf("got %d hits", len(hits))
	}
	if hits[0].id != "a" || hits[1].id != "c" {
		t.Fatalf("ranking wrong: %s, %s, %s", hits[0].id, hits[1].id, hits[2].id)
	}
	if !(hits[0].score >= hits[1].score && hits[1].score >= hits[2].score) {
		t.Fatalf("scores not descending: %v", hits)
	}
}

func TestCleanNameGo(t *testing.T) {
	cases := map[string]string{
		"1234 Space Cats": "Space Cats",
		"-42 Foo":         "Foo",
		"007 Bar":         "Bar",
		"NoNumber Here":   "NoNumber Here",
		"12345":           "12345",
	}
	for in, want := range cases {
		if got := cleanNameGo(in); got != want {
			t.Errorf("cleanNameGo(%q) = %q, want %q", in, got, want)
		}
	}
}

// Mock Ollama: verifies the client's request shape and response parsing without
// a real server or GPU.
func TestOllamaClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embeddings":
			var req struct {
				Model  string `json:"model"`
				Prompt string `json:"prompt"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Model != "bge-m3" || req.Prompt == "" {
				t.Errorf("bad embed request: %+v", req)
			}
			json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{0.1, 0.2, 0.3}})
		case "/api/generate":
			var req struct {
				Model  string   `json:"model"`
				Images []string `json:"images"`
				Stream bool     `json:"stream"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Model != "llava" || len(req.Images) != 1 || req.Stream {
				t.Errorf("bad caption request: %+v", req)
			}
			json.NewEncoder(w).Encode(map[string]any{"response": "  a scene  "})
		case "/api/tags":
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	ol := NewOllama(srv.URL, "bge-m3", "llava")
	ctx := context.Background()

	if !ol.Reachable(ctx) {
		t.Fatal("Reachable = false")
	}
	v, err := ol.Embed(ctx, "hello")
	if err != nil || len(v) != 3 || v[0] != 0.1 {
		t.Fatalf("Embed = %v, %v", v, err)
	}
	cap, err := ol.Caption(ctx, []byte{0xff, 0xd8, 0xff}, "describe")
	if err != nil || cap != "a scene" { // trimmed
		t.Fatalf("Caption = %q, %v", cap, err)
	}
	_ = time.Second
}

// Full worker glue, in-process (no server/ports): build an archive + rows, run
// processOne against a mock Ollama, and confirm it stores durably and ranks.
func TestProcessOneAndSearch(t *testing.T) {
	dir := t.TempDir()
	zpath := filepath.Join(dir, "1 Space Cats.zip")
	zf, err := os.Create(zpath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, _ := zw.Create("01.jpg")
	w.Write([]byte("bytes-sent-to-caption-mock"))
	zw.Close()
	zf.Close()

	index, err := openIndexDB(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := openUserDB(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	index.Exec(`INSERT INTO archives(path, identity, title, page_count, has_cover, status, indexed_at) VALUES(?,?,?,?,?,?,?)`,
		"1 Space Cats.zip", "#1", "1 Space Cats", 1, 1, "indexed", time.Now().Unix())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/embeddings" {
			var req struct {
				Prompt string `json:"prompt"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(map[string]any{"embedding": synonymVec(req.Prompt)})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"response": "cats aboard a spaceship in orbit"})
	}))
	defer srv.Close()

	sem := &Semantic{
		cfg:   Config{RootDir: dir, EmbedModel: "m", VisionModel: "v"},
		index: index, user: user,
		ol:  NewOllama(srv.URL, "m", "v"),
		pos: map[string]int{},
	}
	if err := sem.processOne(candidate{1, "#1", "1 Space Cats.zip"}); err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if sem.count() != 1 {
		t.Fatalf("index count = %d, want 1", sem.count())
	}
	var n int
	user.QueryRow(`SELECT COUNT(*) FROM embeddings WHERE identity='#1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("embedding not persisted (%d)", n)
	}
	var desc string
	user.QueryRow(`SELECT text FROM descriptions WHERE identity='#1'`).Scan(&desc)
	if !strings.Contains(desc, "Space Cats") || !strings.Contains(desc, "spaceship") {
		t.Fatalf("description missing title/caption: %q", desc)
	}
	qv, err := sem.ol.Embed(context.Background(), "cosmic felines")
	if err != nil {
		t.Fatal(err)
	}
	hits := sem.searchVec(qv, 5)
	if len(hits) != 1 || hits[0].id != "#1" {
		t.Fatalf("search hits = %v, want [#1]", hits)
	}
}

func synonymVec(text string) []float64 {
	groups := [][]string{
		{"sea", "ocean", "water", "deep"},
		{"space", "cats", "orbit", "cosmic", "felines", "spaceship", "galaxy"},
		{"mountain", "hike", "climb", "summit"},
	}
	t := strings.ToLower(text)
	v := make([]float64, len(groups))
	for i, ws := range groups {
		for _, word := range ws {
			v[i] += float64(strings.Count(t, word))
		}
	}
	return v
}
