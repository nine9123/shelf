package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
	Category  string `json:"category"`
	CoverVer  int64  `json:"coverVer"` // changes when the cover is (re)generated; for cache-busting
	HasCover  bool   `json:"hasCover"`
	Favorite  bool   `json:"favorite"`
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/archives", a.handleList)
	mux.HandleFunc("GET /api/archives/{id}", a.handleDetail)
	mux.HandleFunc("GET /api/archives/{id}/cover", a.handleCover)
	mux.HandleFunc("PUT /api/archives/{id}/cover", a.handleSetCover)
	mux.HandleFunc("POST /api/archives/{id}/cover/refresh", a.handleRefreshCover)
	mux.HandleFunc("GET /api/archives/{id}/page/{n}", a.handlePage)
	mux.HandleFunc("GET /api/archives/{id}/thumb/{n}", a.handleThumb)
	mux.HandleFunc("PUT /api/archives/{id}/favorite", a.handleFavAdd)
	mux.HandleFunc("DELETE /api/archives/{id}/favorite", a.handleFavDel)
	mux.HandleFunc("PUT /api/archives/{id}/progress", a.handleProgress)
	mux.HandleFunc("GET /api/favorites", a.handleFavorites)
	mux.HandleFunc("POST /api/scan", a.handleScan)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/errors", a.handleErrors)
	mux.HandleFunc("GET /api/id-report", a.handleIDReport)
	mux.HandleFunc("GET /api/namespaces", a.handleNamespaces)
	mux.HandleFunc("GET /api/tags", a.handleTagValues)
	mux.HandleFunc("GET /api/auth/status", a.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/login", a.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", a.handleAuthLogout)
	mux.HandleFunc("GET /api/semantic", a.handleSemantic)
	mux.HandleFunc("GET /api/semantic/status", a.handleSemanticStatus)
	mux.HandleFunc("POST /api/semantic/reset", a.handleSemanticReset)
	mux.HandleFunc("GET /", a.handleIndex)
	return a.withAuth(mux)
}

// cacheScope keeps images out of shared caches once the library is private: an
// authenticated response must not be stored by a proxy and handed to the next
// visitor. Without a key nothing is private, so "public" still applies.
func (a *App) cacheScope() string {
	if a.cfg.APIKey != "" {
		return "private"
	}
	return "public"
}

func (a *App) handleList(w http.ResponseWriter, r *http.Request) {
	favOnly := r.URL.Query().Get("fav") == "1"
	limit := clampInt(atoiDefault(r.URL.Query().Get("limit"), 60), 1, 200)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	terms, tagFilters, category := parseQuery(r.URL.Query().Get("q"))
	favSet := a.favoriteIDSet()

	var sb strings.Builder
	var args []any
	sb.WriteString(`SELECT id, path, identity, title, page_count, has_cover, category, indexed_at FROM archives WHERE status='indexed'`)
	if r.URL.Query().Get("hideDupes") == "1" {
		// Collapse duplicate IDs to a single representative (the lowest row id),
		// so a mistakenly-shared ID shows once instead of cluttering the grid.
		sb.WriteString(` AND id IN (SELECT MIN(id) FROM archives WHERE status='indexed' GROUP BY identity)`)
	}
	for _, term := range terms { // plain words: title / path / info text
		sb.WriteString(` AND (title LIKE ? OR meta LIKE ? OR path LIKE ?)`)
		like := "%" + term + "%"
		args = append(args, like, like, like)
	}
	if category != "" {
		sb.WriteString(` AND category = ? COLLATE NOCASE`)
		args = append(args, category)
	}
	for _, tf := range tagFilters { // each tag filter ANDed via its own subquery
		sb.WriteString(` AND id IN (SELECT archive_id FROM tags WHERE namespace = ? COLLATE NOCASE AND tag = ? COLLATE NOCASE)`)
		args = append(args, tf.ns, tf.val)
	}
	if favOnly {
		idents := a.favoriteIdentities()
		if len(idents) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"items": []archiveItem{}, "limit": limit, "offset": offset})
			return
		}
		sb.WriteString(` AND identity IN (` + placeholders(len(idents)) + `)`)
		for _, id := range idents {
			args = append(args, id)
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
		var identity string
		var hc int
		if err := rows.Scan(&it.ID, &it.Path, &identity, &it.Title, &it.PageCount, &hc, &it.Category, &it.CoverVer); err != nil {
			continue
		}
		it.HasCover = hc == 1
		it.Favorite = favSet[identity]
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
}

func (a *App) handleDetail(w http.ResponseWriter, r *http.Request) {
	var it archiveItem
	var hc int
	var meta, identity, category string
	var coverVer int64
	err := a.index.QueryRow(
		`SELECT id, path, identity, title, page_count, has_cover, meta, category, indexed_at FROM archives WHERE id=? AND status='indexed'`,
		r.PathValue("id")).Scan(&it.ID, &it.Path, &identity, &it.Title, &it.PageCount, &hc, &meta, &category, &coverVer)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Tags grouped by namespace, e.g. {"artist":["foo"],"parody":["bar","baz"]}.
	tags := map[string][]string{}
	if rows, err := a.index.Query(
		`SELECT namespace, tag FROM tags WHERE archive_id=? ORDER BY namespace, tag`, it.ID); err == nil {
		for rows.Next() {
			var ns, tag string
			if rows.Scan(&ns, &tag) == nil {
				tags[ns] = append(tags[ns], tag)
			}
		}
		rows.Close()
	}
	coverPage, coverOverridden := a.coverOverridePage(identity) // page 0 when not overridden
	var description, descModel string
	a.user.QueryRow(`SELECT text, src FROM descriptions WHERE identity=?`, identity).Scan(&description, &descModel)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": it.ID, "path": it.Path, "title": it.Title,
		"pageCount": it.PageCount, "hasCover": hc == 1,
		"favorite": a.isFavorite(identity), "meta": meta,
		"lastPage": a.readProgress(identity),
		"category": category, "tags": tags,
		"coverPage": coverPage, "coverOverridden": coverOverridden, "coverVer": coverVer,
		"description": description, "descriptionModel": descModel,
	})
}

func (a *App) handleTagValues(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("ns")
	limit := clampInt(atoiDefault(r.URL.Query().Get("limit"), 12), 1, 50)
	out := []string{}
	if ns != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(r.URL.Query().Get("prefix"))
		rows, err := a.index.Query(
			`SELECT DISTINCT tag FROM tags WHERE namespace = ? COLLATE NOCASE AND tag LIKE ? ESCAPE '\' COLLATE NOCASE ORDER BY tag LIMIT ?`,
			ns, esc+"%", limit)
		if err == nil {
			for rows.Next() {
				var t string
				if rows.Scan(&t) == nil {
					out = append(out, t)
				}
			}
			rows.Close()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": out})
}

func (a *App) handleSemantic(w http.ResponseWriter, r *http.Request) {
	if !a.sem.enabled() {
		httpErr(w, http.StatusServiceUnavailable, fmt.Errorf("semantic search is disabled (set OLLAMA_URL)"))
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"items": []archiveItem{}})
		return
	}
	limit := clampInt(atoiDefault(r.URL.Query().Get("limit"), 60), 1, 200)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	qv, err := a.sem.ol.Embed(ctx, q)
	if err != nil {
		httpErr(w, http.StatusServiceUnavailable, fmt.Errorf("embed query: %w", err))
		return
	}
	favSet := a.favoriteIDSet()
	items := []archiveItem{}
	for _, h := range a.sem.searchVec(qv, limit) {
		var it archiveItem
		var hc int
		if a.index.QueryRow(
			`SELECT id, path, title, page_count, has_cover, category, indexed_at FROM archives WHERE identity=? AND status='indexed' LIMIT 1`,
			h.id).Scan(&it.ID, &it.Path, &it.Title, &it.PageCount, &hc, &it.Category, &it.CoverVer) == nil {
			it.HasCover = hc == 1
			it.Favorite = favSet[h.id]
			items = append(items, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "query": q})
}

func (a *App) handleSemanticReset(w http.ResponseWriter, r *http.Request) {
	if !a.sem.enabled() {
		httpErr(w, http.StatusServiceUnavailable, fmt.Errorf("semantic search is disabled"))
		return
	}
	// Wipe only the derived semantic data; favorites, progress, and cover
	// overrides in user.db are untouched.
	a.user.Exec(`DELETE FROM embeddings`)
	a.user.Exec(`DELETE FROM descriptions`)
	a.sem.reset()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleSemanticStatus(w http.ResponseWriter, r *http.Request) {
	lastErr, _ := a.sem.lastErr.Load().(string)
	total := 0
	a.index.QueryRow(`SELECT COUNT(*) FROM archives WHERE status='indexed'`).Scan(&total)
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     a.sem.enabled(),
		"embedModel":  a.cfg.EmbedModel,
		"visionModel": a.cfg.VisionModel,
		"embedded":    a.sem.count(),
		"total":       total,
		"working":     a.sem.working.Load(),
		"lastError":   lastErr,
	})
}

func (a *App) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	type ns struct {
		Namespace string `json:"namespace"`
		Count     int    `json:"count"`
	}
	out := []ns{}
	if rows, err := a.index.Query(
		`SELECT namespace, COUNT(*) FROM tags GROUP BY namespace ORDER BY namespace`); err == nil {
		for rows.Next() {
			var n ns
			if rows.Scan(&n.Namespace, &n.Count) == nil {
				out = append(out, n)
			}
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": out})
}

func (a *App) handleCover(w http.ResponseWriter, r *http.Request) {
	var relPath string
	var hc int
	if a.index.QueryRow(`SELECT path, has_cover FROM archives WHERE id=? AND status='indexed'`,
		r.PathValue("id")).Scan(&relPath, &hc) != nil || hc == 0 {
		http.NotFound(w, r) // no row, or no valid cover — don't serve a possibly-stale file
		return
	}
	f := a.coverPath(relPath)
	info, err := os.Stat(f)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// URL is versioned (?v=coverVer), so cache hard; a changed cover is a new URL.
	// ETag is a backstop for revalidation after expiry.
	w.Header().Set("ETag", fmt.Sprintf(`"c%x-%x"`, info.ModTime().UnixNano(), info.Size()))
	w.Header().Set("Cache-Control", a.cacheScope()+", max-age=86400")
	http.ServeFile(w, r, f)
}

// regenCover rebuilds one archive's cached cover from the given page and updates
// has_cover. A stale cover is removed if the chosen page can't be thumbnailed.
func (a *App) regenCover(id string, page int) bool {
	relPath, _, ok := a.lookup(id)
	if !ok {
		return false
	}
	arc, err := openArchive(filepath.Join(a.cfg.RootDir, filepath.FromSlash(relPath)))
	if err != nil {
		return false
	}
	defer arc.Close()
	imgs := arc.Images()
	has := false
	if len(imgs) > 0 {
		if rc, err := arc.Open(imgs[clampInt(page, 0, len(imgs)-1)]); err == nil {
			if makeThumb(rc, a.cfg.ThumbWidth, a.coverPath(relPath)) == nil {
				has = true
			}
			rc.Close()
		}
	}
	if !has {
		os.Remove(a.coverPath(relPath))
	}
	a.index.Exec(`UPDATE archives SET has_cover=?, indexed_at=? WHERE id=?`, b2i(has), time.Now().Unix(), id)
	return has
}

func (a *App) handleSetCover(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identityForID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Page int `json:"page"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Page < 0 {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	a.user.Exec(`INSERT INTO cover_override(identity, page, updated_at) VALUES(?, ?, ?)
ON CONFLICT(identity) DO UPDATE SET page=excluded.page, updated_at=excluded.updated_at`,
		identity, body.Page, time.Now().Unix())
	has := a.regenCover(r.PathValue("id"), body.Page)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "page": body.Page, "hasCover": has})
}

func (a *App) handleRefreshCover(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identityForID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	page, _ := a.coverOverridePage(identity) // 0 when there's no override
	has := a.regenCover(r.PathValue("id"), page)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hasCover": has})
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
	w.Header().Set("Cache-Control", a.cacheScope()+", max-age=86400")
	io.Copy(w, rc)
}

func (a *App) handleThumb(w http.ResponseWriter, r *http.Request) {
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
	etag := fmt.Sprintf(`"t%s-%d-%d"`, r.PathValue("id"), mt, n)
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
	// Decode straight from the entry stream; only the small JPEG is buffered,
	// so nothing is written until we know which response we're sending.
	var buf bytes.Buffer
	thumbErr := makeThumbTo(io.LimitReader(rc, 48<<20), 240, &buf) // cap 48 MB per page
	rc.Close()

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", a.cacheScope()+", max-age=86400")
	if thumbErr == nil {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(buf.Bytes())
		return
	}
	// couldn't decode/resize (e.g. AVIF/JXL) — reopen the entry and stream the
	// original instead, as the page endpoint does
	rc, err = arc.Open(imgs[n])
	if err != nil {
		w.Header().Del("ETag")
		w.Header().Del("Cache-Control")
		http.Error(w, "cannot read page", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	ct := ctByExt[strings.ToLower(path.Ext(imgs[n]))]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	io.Copy(w, rc)
}

func (a *App) handleFavAdd(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identityForID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := a.user.Exec(
		`INSERT OR IGNORE INTO favorites(identity, created_at) VALUES(?, ?)`,
		identity, time.Now().Unix()); err != nil {
		httpErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"favorite": true})
}

func (a *App) handleFavDel(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identityForID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.user.Exec(`DELETE FROM favorites WHERE identity=?`, identity)
	writeJSON(w, http.StatusOK, map[string]any{"favorite": false})
}

func (a *App) handleFavorites(w http.ResponseWriter, r *http.Request) {
	items := []archiveItem{}
	for _, ident := range a.favoriteIdentities() {
		var it archiveItem
		var hc int
		// A favorited identity may map to more than one path only if IDs clash;
		// pick the first indexed match for display.
		err := a.index.QueryRow(
			`SELECT id, path, title, page_count, has_cover FROM archives WHERE identity=? AND status='indexed' ORDER BY path LIMIT 1`,
			ident).Scan(&it.ID, &it.Path, &it.Title, &it.PageCount, &hc)
		if err != nil {
			continue // favorited but not currently indexed (e.g. file removed)
		}
		it.HasCover = hc == 1
		it.Favorite = true
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleProgress(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identityForID(r.PathValue("id"))
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
INSERT INTO read_progress(identity, last_page, updated_at) VALUES(?, ?, ?)
ON CONFLICT(identity) DO UPDATE SET last_page=excluded.last_page, updated_at=excluded.updated_at`,
		identity, body.Page, time.Now().Unix()); err != nil {
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

func (a *App) handleIDReport(w http.ResponseWriter, r *http.Request) {
	// Numeric IDs claimed by more than one indexed archive (ambiguous favorites).
	// Include size and page count so you can judge whether they're the same story.
	type dupFile struct {
		Path      string `json:"path"`
		Size      int64  `json:"size"`
		PageCount int    `json:"pageCount"`
	}
	dupMap := map[string][]dupFile{}
	if rows, err := a.index.Query(`
SELECT identity, path, size, page_count FROM archives
WHERE status='indexed' AND identity IN (
    SELECT identity FROM archives WHERE status='indexed' AND identity LIKE '#%'
    GROUP BY identity HAVING COUNT(*) > 1)
ORDER BY identity, path`); err == nil {
		for rows.Next() {
			var id string
			var f dupFile
			if rows.Scan(&id, &f.Path, &f.Size, &f.PageCount) == nil {
				dupMap[id] = append(dupMap[id], f)
			}
		}
		rows.Close()
	}
	duplicates := []map[string]any{}
	for id, files := range dupMap {
		duplicates = append(duplicates, map[string]any{"id": id, "files": files})
	}

	// Archives with no leading "<digits> " ID — these fall back to path identity
	// and won't survive a move until you give them an ID.
	missing := []string{}
	if rows, err := a.index.Query(
		`SELECT path FROM archives WHERE status='indexed' AND identity LIKE '@%' ORDER BY path`); err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				missing = append(missing, p)
			}
		}
		rows.Close()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"duplicates": duplicates, "duplicateCount": len(duplicates),
		"missing": missing, "missingCount": len(missing),
	})
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

func (a *App) coverOverrides() map[string]int {
	m := map[string]int{}
	rows, err := a.user.Query(`SELECT identity, page FROM cover_override`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p int
		if rows.Scan(&id, &p) == nil {
			m[id] = p
		}
	}
	return m
}

func (a *App) coverOverridePage(identity string) (int, bool) {
	var p int
	if a.user.QueryRow(`SELECT page FROM cover_override WHERE identity=?`, identity).Scan(&p) != nil {
		return 0, false
	}
	return p, true
}

func (a *App) favoriteIdentities() []string {
	rows, err := a.user.Query(`SELECT identity FROM favorites ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func (a *App) favoriteIDSet() map[string]bool {
	m := map[string]bool{}
	for _, id := range a.favoriteIdentities() {
		m[id] = true
	}
	return m
}

// identityForID resolves an archive's numeric row id to its durable identity.
func (a *App) identityForID(id string) (string, bool) {
	var identity string
	if a.index.QueryRow(`SELECT identity FROM archives WHERE id=? AND status='indexed'`, id).
		Scan(&identity) != nil {
		return "", false
	}
	return identity, true
}

func (a *App) isFavorite(identity string) bool {
	var one int
	return a.user.QueryRow(`SELECT 1 FROM favorites WHERE identity=?`, identity).Scan(&one) == nil
}

func (a *App) readProgress(identity string) int {
	var p int
	if a.user.QueryRow(`SELECT last_page FROM read_progress WHERE identity=?`, identity).Scan(&p) != nil {
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
