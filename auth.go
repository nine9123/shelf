package main

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Auth is deliberately the smallest thing that works for a single-user,
// self-hosted app: one shared key in API_KEY. Set it and the library is
// private; leave it unset and Shelf is wide open, exactly as before.
//
// The browser trades that key for a cookie at /api/auth/login. That indirection
// exists because the reader is mostly <img src="/api/…"> — image requests can't
// carry an Authorization header, so a header-only scheme would leave every page
// and cover unreadable. The key itself lives in localStorage so a returning
// visitor can silently re-establish the cookie instead of typing it again.
//
// Scripts and curl can skip the dance and send X-API-Key or a Bearer token.
const (
	sessionCookie = "shelf_key"
	sessionMaxAge = 30 * 24 * time.Hour
)

// publicPaths are reachable without a key: the HTML shell (it holds no library
// data, and the login form has to render somewhere), its icons, the auth
// endpoints, and the readiness check (orchestrators send no key).
var publicPaths = map[string]bool{
	"/":                     true,
	"/favicon.svg":          true,
	"/favicon.ico":          true,
	"/apple-touch-icon.png": true,
	"/healthz":              true,
	"/api/auth/status":      true,
	"/api/auth/login":       true,
	"/api/auth/logout":      true,
}

// withAuth gates everything except publicPaths. With no API_KEY configured it
// returns the handler untouched, so the unauthenticated deployment costs
// nothing at request time.
func (a *App) withAuth(next http.Handler) http.Handler {
	if a.cfg.APIKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[r.URL.Path] || a.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	})
}

func (a *App) authorized(r *http.Request) bool {
	return a.checkKey(keyFromRequest(r))
}

// checkKey compares in constant time so a wrong key leaks nothing about how
// much of it was right. Always true when auth is disabled.
func (a *App) checkKey(k string) bool {
	if a.cfg.APIKey == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(k), []byte(a.cfg.APIKey)) == 1
}

// keyFromRequest looks in the two places a client can put the key: a header
// (scripts) or the session cookie (browser).
func keyFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); v != "" {
		if k, ok := cutPrefixFold(v, "Bearer "); ok {
			return strings.TrimSpace(k)
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return decodeCookieValue(c.Value)
	}
	return ""
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// The cookie carries the key base64-encoded: net/http silently drops bytes that
// aren't legal in a cookie value (spaces, commas, quotes), which would quietly
// corrupt an otherwise fine key.
func encodeCookieValue(k string) string { return base64.RawURLEncoding.EncodeToString([]byte(k)) }

func decodeCookieValue(v string) string {
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func (a *App) sessionCookieFor(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		// Lax is what makes the cookie safe to use for state-changing requests:
		// a cross-site POST/PUT/DELETE won't carry it, so no CSRF token is needed.
		SameSite: http.SameSiteLaxMode,
		// Only when the connection really is HTTPS. Setting Secure on plain HTTP
		// makes the browser discard the cookie, which would break login outright
		// for the common "http://nas:8080" deployment.
		Secure: isTLS(r),
	}
}

func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Spoofable, but the only thing it can change is whether Secure is set, and
	// the safe-by-default choice (setting it) is the one that breaks plain HTTP.
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// --- handlers ---

// handleAuthStatus tells the UI whether to show the login gate at all.
func (a *App) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"required":      a.cfg.APIKey != "",
		"authenticated": a.cfg.APIKey == "" || a.authorized(r),
	})
}

func (a *App) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.cfg.APIKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "required": false})
		return
	}
	ip := clientIP(r)
	if a.logins.blocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many attempts"})
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !a.checkKey(strings.TrimSpace(body.Key)) {
		a.logins.fail(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid key"})
		return
	}
	a.logins.reset(ip)
	http.SetCookie(w, a.sessionCookieFor(r, encodeCookieValue(a.cfg.APIKey), int(sessionMaxAge.Seconds())))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, a.sessionCookieFor(r, "", -1))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- brute-force throttle ---

const (
	loginMaxFails = 8
	loginWindow   = 5 * time.Minute
	loginMaxIPs   = 1024
)

// loginLimiter gives a client a handful of guesses per window and then stops
// answering. Keyed on RemoteAddr rather than X-Forwarded-For: that header is
// attacker-controlled, so trusting it would let a guesser reset the counter on
// every request. Behind a reverse proxy every client shares one bucket — the
// cooldown is short enough that this is an annoyance, not a lockout.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	n    int
	seen time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{fails: map[string]*failRecord{}}
}

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.fails[ip]
	if !ok {
		return false
	}
	if time.Since(rec.seen) > loginWindow {
		delete(l.fails, ip)
		return false
	}
	return rec.n >= loginMaxFails
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	rec, ok := l.fails[ip]
	if !ok || now.Sub(rec.seen) > loginWindow {
		rec = &failRecord{}
		l.fails[ip] = rec
	}
	rec.n++
	rec.seen = now
	if len(l.fails) > loginMaxIPs {
		for k, v := range l.fails {
			if now.Sub(v.seen) > loginWindow {
				delete(l.fails, k)
			}
		}
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
