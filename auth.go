package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth is deliberately the smallest thing that works for a single-user,
// self-hosted app: one shared key in API_KEY. Set it and the library is
// private; leave it unset and Shelf is wide open, exactly as before.
//
// The browser trades that key for a cookie at /api/auth/login. The cookie holds
// a signed, expiring session token — never the key itself — so a leaked cookie
// is good for at most sessionMaxAge and can't be turned back into the key. That
// indirection exists because the reader is mostly <img src="/api/…"> — image requests can't
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
		setSecurityHeaders(w)
		if !sameOrigin(r) {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request refused"})
			return
		}
		if publicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		switch a.authorize(r) {
		case authOK:
			next.ServeHTTP(w, r)
			return
		case authThrottled:
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many attempts"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	})
}

type authResult int

const (
	authDenied authResult = iota
	authOK
	authThrottled
)

// authorize checks the two places a client can put a credential: a header
// carrying the key (scripts) or the session cookie (browser).
//
// A key sent in a header is a guess like any other, so it goes through the
// same per-client throttle as the login form — otherwise the 8-per-5-minutes
// limit could be sidestepped by guessing via X-API-Key on any endpoint. Cookies
// aren't counted: a session token is a 256-bit MAC, not something you can
// guess, and a browser holding a stale one fires a burst of failing <img>
// requests that must not lock its owner out of logging in again.
func (a *App) authorize(r *http.Request) authResult {
	if k, ok := keyFromHeader(r); ok {
		ip := clientIP(r)
		if a.logins.blocked(ip) {
			return authThrottled
		}
		if a.checkKey(k) {
			return authOK
		}
		a.logins.fail(ip)
		return authDenied
	}
	if c, err := r.Cookie(sessionCookie); err == nil && a.checkSession(c.Value, time.Now()) {
		return authOK
	}
	return authDenied
}

// checkKey compares SHA-256 digests in constant time. Hashing first means both
// sides are always 32 bytes, so neither how much of a guess was right nor how
// long the real key is leaks through timing. Always true when auth is disabled.
func (a *App) checkKey(k string) bool {
	if a.cfg.APIKey == "" {
		return true
	}
	got := sha256.Sum256([]byte(k))
	want := sha256.Sum256([]byte(a.cfg.APIKey))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// keyFromHeader returns a key sent as X-API-Key or "Authorization: Bearer".
func keyFromHeader(r *http.Request) (string, bool) {
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v, true
	}
	if v := r.Header.Get("Authorization"); v != "" {
		if k, ok := cutPrefixFold(v, "Bearer "); ok {
			return strings.TrimSpace(k), true
		}
	}
	return "", false
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// --- session tokens ---
//
// A session token is "<expiry unix seconds>.<base64url HMAC-SHA256>", the MAC
// keyed by a value derived from API_KEY. It's stateless, so it survives
// restarts (idlegate stops Shelf whenever it's idle) with nothing to persist,
// and rotating API_KEY invalidates every outstanding session at once. Only
// cookie-safe characters are used, so net/http never mangles it.

const sessionTokenPrefix = "v1."

func (a *App) sessionMAC(exp string) []byte {
	// Derive a separate MAC key rather than using API_KEY directly, so the
	// token never doubles as an oracle for the key itself.
	k := sha256.Sum256([]byte("shelf-session-key\x00" + a.cfg.APIKey))
	m := hmac.New(sha256.New, k[:])
	m.Write([]byte(sessionTokenPrefix + exp))
	return m.Sum(nil)
}

func (a *App) newSession(now time.Time) string {
	exp := strconv.FormatInt(now.Add(sessionMaxAge).Unix(), 10)
	return sessionTokenPrefix + exp + "." + base64.RawURLEncoding.EncodeToString(a.sessionMAC(exp))
}

func (a *App) checkSession(tok string, now time.Time) bool {
	rest, ok := strings.CutPrefix(tok, sessionTokenPrefix)
	if !ok {
		return false
	}
	exp, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return false
	}
	expUnix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.Unix() >= expUnix {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(got, a.sessionMAC(exp))
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

// setSecurityHeaders is applied to every response once auth is on. The login
// form and the Lock button must not be framed by another site (clickjacking),
// archive bytes must never be sniffed into something executable, and URLs —
// which include search queries — shouldn't leak to other sites via Referer.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("Referrer-Policy", "same-origin")
}

// sameOrigin refuses state-changing requests that a browser sent on behalf of
// another site. SameSite=Lax already keeps the cookie off cross-site requests,
// but "site" is coarser than "origin": another app on the same host (say
// http://nas:8081) is same-site, so its pages could still POST here with the
// cookie attached. Safe methods are always allowed; so are requests without
// browser provenance headers (curl, scripts), which can't ride a cookie anyway.
func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	// Every current browser sends Sec-Fetch-Site, and it's computed by the
	// browser itself, so it holds even when a reverse proxy rewrites Host.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) ||
		strings.EqualFold(u.Host, r.Header.Get("X-Forwarded-Host"))
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

// handleAuthStatus tells the UI whether to show the login gate at all. It goes
// through authorize, throttle included: the endpoint is public, so a bare key
// check here would be an unthrottled way to test keys.
func (a *App) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"required":      a.cfg.APIKey != "",
		"authenticated": a.cfg.APIKey == "" || a.authorize(r) == authOK,
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
	http.SetCookie(w, a.sessionCookieFor(r, a.newSession(time.Now()), int(sessionMaxAge.Seconds())))
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
//
// IPv6 clients are bucketed by /64: that's what a single host or home network
// is usually handed, so rotating through its addresses buys no extra guesses.
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
	if rec.n == loginMaxFails {
		log.Printf("auth: %d failed key attempts from %s, refusing it for %s", rec.n, ip, loginWindow)
	}
	if len(l.fails) > loginMaxIPs {
		for k, v := range l.fails {
			if now.Sub(v.seen) > loginWindow {
				delete(l.fails, k)
			}
		}
	}
	// Still over the cap with every entry live: drop arbitrary ones (never the
	// one just recorded) so a flood of addresses can't grow memory without bound.
	for k := range l.fails {
		if len(l.fails) <= loginMaxIPs {
			break
		}
		if k != ip {
			delete(l.fails, k)
		}
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return host
}
