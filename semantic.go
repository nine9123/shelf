package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const captionPrompt = "Describe this manga/comic page for a search index in one or two concise sentences: the setting, the characters (how many, appearance), the mood, and what is happening. State only concrete visual details. Do not refuse or add commentary."

// Semantic owns the durable description/embedding store, an in-memory vector
// index for cosine search, and a background worker that populates it via Ollama.
// Everything is keyed by archive identity, so it survives cache rebuilds, moves,
// renames, and re-encodes.
type Semantic struct {
	cfg   Config
	index *sql.DB
	user  *sql.DB
	ol    *Ollama

	mu   sync.RWMutex
	ids  []string      // identities, parallel to vecs
	vecs [][]float32   // unit-normalized vectors
	pos  map[string]int

	working atomic.Bool
	lastErr atomic.Value // string
}

func (s *Semantic) enabled() bool { return s != nil && s.ol != nil }

// LoadIndex reads all stored embeddings into memory at startup.
func (s *Semantic) LoadIndex() {
	if s == nil {
		return
	}
	rows, err := s.user.Query(`SELECT identity, vec FROM embeddings`)
	if err != nil {
		return
	}
	defer rows.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pos = map[string]int{}
	for rows.Next() {
		var id string
		var b sql.RawBytes // decodeVec copies it, so skip the driver's own copy
		if rows.Scan(&id, &b) == nil {
			v := decodeVec(b)
			normalizeInPlace(v) // v is ours; no second copy per vector
			s.pos[id] = len(s.ids)
			s.ids = append(s.ids, id)
			s.vecs = append(s.vecs, v)
		}
	}
}

type scored struct {
	id    string
	score float32
}

// searchVec ranks stored vectors by cosine similarity to q.
func (s *Semantic) searchVec(q []float32, n int) []scored {
	q = normalizeVec(q)
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]scored, len(s.ids))
	for i, id := range s.ids {
		res[i] = scored{id, dotVec(q, s.vecs[i])}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].score > res[j].score })
	if len(res) > n {
		res = res[:n]
	}
	return res
}

func (s *Semantic) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ids)
}

func (s *Semantic) addVec(id string, vec []float32) {
	nv := normalizeVec(vec)
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.pos[id]; ok {
		s.vecs[i] = nv
		return
	}
	s.pos[id] = len(s.ids)
	s.ids = append(s.ids, id)
	s.vecs = append(s.vecs, nv)
}

func (s *Semantic) has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.pos[id]
	return ok
}

// reset clears the in-memory index (after the durable tables are wiped) so the
// worker re-captions and re-embeds everything, e.g. after pulling a better model.
func (s *Semantic) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = nil
	s.vecs = nil
	s.pos = map[string]int{}
}

// Run is the background worker. It captions covers and embeds descriptions,
// low-priority and incrementally, so search works over whatever's done so far.
func (s *Semantic) Run() {
	if !s.enabled() {
		return
	}
	for {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		ok := s.ol.Reachable(rctx)
		rcancel()
		if !ok {
			s.lastErr.Store("Ollama not reachable at " + s.cfg.OllamaURL)
			time.Sleep(30 * time.Second)
			continue
		}
		// Require the vision model to be present, so a missing model stalls with a
		// clear message rather than silently producing caption-less embeddings.
		if s.cfg.VisionModel != "" {
			vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Second)
			has := s.ol.HasModel(vctx, s.cfg.VisionModel)
			vcancel()
			if !has {
				s.lastErr.Store("vision model \"" + s.cfg.VisionModel + "\" not found — run: ollama pull " + s.cfg.VisionModel)
				time.Sleep(30 * time.Second)
				continue
			}
		}
		batch := s.nextBatch(16)
		if len(batch) == 0 {
			time.Sleep(60 * time.Second) // nothing pending; poll for newly-added archives
			continue
		}
		s.working.Store(true)
		for _, c := range batch {
			if err := s.processOne(c); err != nil {
				s.lastErr.Store(err.Error())
				time.Sleep(5 * time.Second)
			} else {
				s.lastErr.Store("")
			}
			if s.cfg.SemanticDelay > 0 {
				time.Sleep(s.cfg.SemanticDelay)
			}
		}
		s.working.Store(false)
	}
}

type candidate struct {
	id       int64
	identity string
	path     string
}

// nextBatch returns indexed archives that still need embedding, favorites first.
func (s *Semantic) nextBatch(limit int) []candidate {
	var out []candidate
	seen := map[string]bool{}

	add := func(id int64, identity, path string) bool {
		if identity == "" || seen[identity] || s.has(identity) {
			return false
		}
		seen[identity] = true
		out = append(out, candidate{id, identity, path})
		return len(out) >= limit
	}

	// favorites first
	if rows, err := s.user.Query(`SELECT identity FROM favorites`); err == nil {
		var favs []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				favs = append(favs, id)
			}
		}
		rows.Close()
		for _, fid := range favs {
			var id int64
			var path string
			if s.index.QueryRow(`SELECT id, path FROM archives WHERE identity=? AND status='indexed' LIMIT 1`, fid).
				Scan(&id, &path) == nil {
				if add(id, fid, path) {
					return out
				}
			}
		}
	}

	// then everything else, newest first
	rows, err := s.index.Query(`SELECT id, identity, path FROM archives WHERE status='indexed' ORDER BY indexed_at DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var identity, path string
		if rows.Scan(&id, &identity, &path) == nil {
			if add(id, identity, path) {
				return out
			}
		}
	}
	return out
}

func (s *Semantic) processOne(c candidate) error {
	caption := s.captionCover(c.path)
	title, category, meta, tags := s.archiveText(c.id)
	text := buildDescription(caption, cleanNameGo(title), category, tags, meta)

	vec, err := func() ([]float32, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return s.ol.Embed(ctx, text)
	}()
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	s.user.Exec(`INSERT INTO descriptions(identity, text, src, updated_at) VALUES(?,?,?,?)
ON CONFLICT(identity) DO UPDATE SET text=excluded.text, src=excluded.src, updated_at=excluded.updated_at`,
		c.identity, text, s.cfg.VisionModel, now)
	s.user.Exec(`INSERT INTO embeddings(identity, model, dim, vec, updated_at) VALUES(?,?,?,?,?)
ON CONFLICT(identity) DO UPDATE SET model=excluded.model, dim=excluded.dim, vec=excluded.vec, updated_at=excluded.updated_at`,
		c.identity, s.cfg.EmbedModel, len(vec), encodeVec(vec), now)
	s.addVec(c.identity, vec)
	return nil
}

// captionCover opens the archive's first page and asks the vision model to
// describe it. Returns "" on any failure (embedding still proceeds on metadata).
func (s *Semantic) captionCover(relPath string) string {
	abs := filepath.Join(s.cfg.RootDir, filepath.FromSlash(relPath))
	arc, err := openArchive(abs)
	if err != nil {
		return ""
	}
	defer arc.Close()
	imgs := arc.Images()
	if len(imgs) == 0 {
		return ""
	}
	rc, err := arc.Open(imgs[0])
	if err != nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(rc, 32<<20))
	rc.Close()
	// Normalize to a modest JPEG: this fixes vision models that can't decode WebP
	// and friends, and shrinks the image so encoding is much faster. If we can't
	// decode it (e.g. AVIF/JXL), skip the caption and embed on metadata alone.
	var jpg bytes.Buffer
	if makeThumbTo(bytes.NewReader(raw), 1024, &jpg) != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cap, err := s.ol.Caption(ctx, jpg.Bytes(), captionPrompt)
	if err != nil {
		return ""
	}
	return cap
}

func (s *Semantic) archiveText(id int64) (title, category, meta string, tags []string) {
	s.index.QueryRow(`SELECT title, category, meta FROM archives WHERE id=?`, id).Scan(&title, &category, &meta)
	rows, err := s.index.Query(`SELECT namespace, tag FROM tags WHERE archive_id=? ORDER BY namespace, tag`, id)
	if err == nil {
		for rows.Next() {
			var ns, tag string
			if rows.Scan(&ns, &tag) == nil {
				tags = append(tags, ns+":"+tag)
			}
		}
		rows.Close()
	}
	return
}

func buildDescription(caption, title, category string, tags []string, meta string) string {
	var parts []string
	if title != "" {
		parts = append(parts, title)
	}
	if category != "" {
		parts = append(parts, "Category: "+category)
	}
	if len(tags) > 0 {
		parts = append(parts, "Tags: "+strings.Join(tags, ", "))
	}
	if caption != "" {
		parts = append(parts, caption)
	}
	if meta != "" {
		if len(meta) > 600 {
			meta = meta[:600]
		}
		parts = append(parts, meta)
	}
	return strings.Join(parts, "\n")
}

// --- vector helpers ---

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

func normalizeVec(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(s))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// normalizeInPlace scales v to unit length without allocating.
func normalizeInPlace(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
}

func dotVec(a, b []float32) float32 {
	if len(a) != len(b) {
		return -1
	}
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// cleanNameGo mirrors the UI's clean-name: strip a leading "<id> " prefix.
func cleanNameGo(s string) string {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > start && i < len(s) && s[i] == ' ' {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

