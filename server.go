package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ctByExt keeps content-type detection deterministic regardless of the host's
// mime database.
var ctByExt = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
}

type archiveItem struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	Title     string `json:"title"`
	PageCount int    `json:"pageCount"`
	HasCover  bool   `json:"hasCover"`
	Favorite  bool   `json:"favorite"`
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/archives", a.handleList)
	mux.HandleFunc("GET /api/archives/{id}", a.handleDetail)
	mux.HandleFunc("GET /api/archives/{id}/cover", a.handleCover)
	mux.HandleFunc("GET /api/archives/{id}/page/{n}", a.handlePage)
	mux.HandleFunc("PUT /api/archives/{id}/favorite", a.handleFavAdd)
	mux.HandleFunc("DELETE /api/archives/{id}/favorite", a.handleFavDel)
	mux.HandleFunc("PUT /api/archives/{id}/progress", a.handleProgress)
	mux.HandleFunc("GET /api/favorites", a.handleFavorites)
	mux.HandleFunc("POST /api/scan", a.handleScan)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/errors", a.handleErrors)
	mux.HandleFunc("GET /", a.handleIndex)
	return mux
}

func (a *App) handleList(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	favOnly := r.URL.Query().Get("fav") == "1"
	limit := clampInt(atoiDefault(r.URL.Query().Get("limit"), 60), 1, 200)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	favSet := a.favoriteSet()

	var sb strings.Builder
	var args []any
	sb.WriteString(`SELECT id, path, title, page_count, has_cover FROM archives WHERE status='indexed'`)
	if q != "" {
		sb.WriteString(` AND (title LIKE ? OR meta LIKE ? OR path LIKE ?)`)
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	if favOnly {
		paths := a.favoritePaths()
		if len(paths) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"items": []archiveItem{}, "limit": limit, "offset": offset})
			return
		}
		sb.WriteString(` AND path IN (` + placeholders(len(paths)) + `)`)
		for _, p := range paths {
			args = append(args, p)
		}
	}
	sb.WriteString(` ORDER BY title COLLATE NOCASE LIMIT ? OFFSET ?`)
	args = append(args, limit, offset)

	rows, err := a.index.Query(sb.String(), args...)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := []archiveItem{}
	for rows.Next() {
		var it archiveItem
		var hc int
		if err := rows.Scan(&it.ID, &it.Path, &it.Title, &it.PageCount, &hc); err != nil {
			continue
		}
		it.HasCover = hc == 1
		it.Favorite = favSet[it.Path]
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
}

func (a *App) handleDetail(w http.ResponseWriter, r *http.Request) {
	var it archiveItem
	var hc int
	var meta string
	err := a.index.QueryRow(
		`SELECT id, path, title, page_count, has_cover, meta FROM archives WHERE id=? AND status='indexed'`,
		r.PathValue("id")).Scan(&it.ID, &it.Path, &it.Title, &it.PageCount, &hc, &meta)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": it.ID, "path": it.Path, "title": it.Title,
		"pageCount": it.PageCount, "hasCover": hc == 1,
		"favorite": a.isFavorite(it.Path), "meta": meta,
		"lastPage": a.readProgress(it.Path),
	})
}

func (a *App) handleCover(w http.ResponseWriter, r *http.Request) {
	relPath, _, ok := a.lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, a.coverPath(relPath)) // 404s cleanly if the cover is missing
}

func (a *App) handlePage(w http.ResponseWriter, r *http.Request) {
	relPath, mt, ok := a.lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 {
		http.Error(w, "bad page number", http.StatusBadRequest)
		return
	}
	etag := fmt.Sprintf(`"%s-%d-%d"`, r.PathValue("id"), mt, n)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	abs := filepath.Join(a.cfg.RootDir, filepath.FromSlash(relPath))
	arc, err := openArchive(abs)
	if err != nil {
		http.Error(w, "cannot open archive", http.StatusInternalServerError)
		return
	}
	defer arc.Close()
	imgs := arc.Images()
	if n >= len(imgs) {
		http.NotFound(w, r)
		return
	}
	rc, err := arc.Open(imgs[n])
	if err != nil {
		http.Error(w, "cannot read page", http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	ct := ctByExt[strings.ToLower(path.Ext(imgs[n]))]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.Copy(w, rc)
}

func (a *App) handleFavAdd(w http.ResponseWriter, r *http.Request) {
	relPath, _, ok := a.lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := a.user.Exec(
		`INSERT OR IGNORE INTO favorites(archive_path, created_at) VALUES(?, ?)`,
		relPath, time.Now().Unix()); err != nil {
		httpErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"favorite": true})
}

func (a *App) handleFavDel(w http.ResponseWriter, r *http.Request) {
	relPath, _, ok := a.lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.user.Exec(`DELETE FROM favorites WHERE archive_path=?`, relPath)
	writeJSON(w, http.StatusOK, map[string]any{"favorite": false})
}

func (a *App) handleFavorites(w http.ResponseWriter, r *http.Request) {
	items := []archiveItem{}
	for _, p := range a.favoritePaths() {
		var it archiveItem
		var hc int
		err := a.index.QueryRow(
			`SELECT id, path, title, page_count, has_cover FROM archives WHERE path=? AND status='indexed'`,
			p).Scan(&it.ID, &it.Path, &it.Title, &it.PageCount, &hc)
		if err != nil {
			continue // favorited but not currently indexed (e.g. file moved away)
		}
		it.HasCover = hc == 1
		it.Favorite = true
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleProgress(w http.ResponseWriter, r *http.Request) {
	relPath, _, ok := a.lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Page int `json:"page"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if _, err := a.user.Exec(`
INSERT INTO read_progress(archive_path, last_page, updated_at) VALUES(?, ?, ?)
ON CONFLICT(archive_path) DO UPDATE SET last_page=excluded.last_page, updated_at=excluded.updated_at`,
		relPath, body.Page, time.Now().Unix()); err != nil {
		httpErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleScan(w http.ResponseWriter, r *http.Request) {
	if a.scanning.Load() {
		writeJSON(w, http.StatusOK, map[string]any{"status": "already running"})
		return
	}
	go a.Scan()
	writeJSON(w, http.StatusOK, map[string]any{"status": "started"})
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	if rows, err := a.index.Query(`SELECT status, COUNT(*) FROM archives GROUP BY status`); err == nil {
		for rows.Next() {
			var s string
			var c int
			if rows.Scan(&s, &c) == nil {
				counts[s] = c
			}
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scanning": a.scanning.Load(),
		"lastScan": a.lastScan.Load(),
		"counts":   counts,
	})
}

func (a *App) handleErrors(w http.ResponseWriter, r *http.Request) {
	limit := clampInt(atoiDefault(r.URL.Query().Get("limit"), 100), 1, 1000)
	rows, err := a.index.Query(
		`SELECT path, error FROM archives WHERE status='error' ORDER BY path LIMIT ?`, limit)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	type errItem struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	}
	items := []errItem{}
	for rows.Next() {
		var it errItem
		if rows.Scan(&it.Path, &it.Error) == nil {
			items = append(items, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// --- helpers ---

func (a *App) lookup(id string) (relPath string, mtime int64, ok bool) {
	if err := a.index.QueryRow(
		`SELECT path, mtime FROM archives WHERE id=? AND status='indexed'`, id).
		Scan(&relPath, &mtime); err != nil {
		return "", 0, false
	}
	return relPath, mtime, true
}

func (a *App) favoritePaths() []string {
	rows, err := a.user.Query(`SELECT archive_path FROM favorites ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out
}

func (a *App) favoriteSet() map[string]bool {
	m := map[string]bool{}
	for _, p := range a.favoritePaths() {
		m[p] = true
	}
	return m
}

func (a *App) isFavorite(relPath string) bool {
	var one int
	return a.user.QueryRow(`SELECT 1 FROM favorites WHERE archive_path=?`, relPath).Scan(&one) == nil
}

func (a *App) readProgress(relPath string) int {
	var p int
	if a.user.QueryRow(`SELECT last_page FROM read_progress WHERE archive_path=?`, relPath).Scan(&p) != nil {
		return 0
	}
	return p
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, err error) {
	log.Printf("http %d: %v", code, err)
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
