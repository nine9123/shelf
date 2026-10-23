package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testKey = "s3cret-key-with spaces,and;punctuation"

func testApp(key string) *App {
	return &App{cfg: Config{APIKey: key}, logins: newLoginLimiter()}
}

// guarded wraps a handler that records whether it was reached at all.
func guarded(a *App, reached *bool) http.Handler {
	return a.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}))
}

func TestWithAuthDisabledLetsEverythingThrough(t *testing.T) {
	a := testApp("")
	var reached bool
	rec := httptest.NewRecorder()
	guarded(a, &reached).ServeHTTP(rec, httptest.NewRequest("GET", "/api/archives", nil))
	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("reached=%v code=%d, want true/200", reached, rec.Code)
	}
}

func TestWithAuthGate(t *testing.T) {
	cookie := &http.Cookie{Name: sessionCookie, Value: testApp(testKey).newSession(time.Now())}
	tests := []struct {
		name   string
		path   string
		set    func(*http.Request)
		want   int
		passed bool
	}{
		{"no credentials", "/api/archives", nil, http.StatusUnauthorized, false},
		{"wrong key", "/api/archives",
			func(r *http.Request) { r.Header.Set("X-API-Key", "nope") }, http.StatusUnauthorized, false},
		{"key prefix only", "/api/archives",
			func(r *http.Request) { r.Header.Set("X-API-Key", testKey[:5]) }, http.StatusUnauthorized, false},
		{"api key header", "/api/archives",
			func(r *http.Request) { r.Header.Set("X-API-Key", testKey) }, http.StatusOK, true},
		{"bearer token", "/api/archives",
			func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testKey) }, http.StatusOK, true},
		{"bearer token odd case", "/api/archives",
			func(r *http.Request) { r.Header.Set("Authorization", "bEaRer "+testKey) }, http.StatusOK, true},
		{"session cookie", "/api/archives",
			func(r *http.Request) { r.AddCookie(cookie) }, http.StatusOK, true},
		{"garbage cookie", "/api/archives",
			func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "!!!"}) },
			http.StatusUnauthorized, false},
		// What older versions stored: the key itself, base64'd. No longer a session.
		{"legacy key cookie", "/api/archives",
			func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(testKey))})
			},
			http.StatusUnauthorized, false},
		// The HTML shell has to render for the login form to exist.
		{"index is public", "/", nil, http.StatusOK, true},
		{"auth status is public", "/api/auth/status", nil, http.StatusOK, true},
		{"healthz is public", "/healthz", nil, http.StatusOK, true},
		{"favicon is public", "/favicon.svg", nil, http.StatusOK, true},
		{"favicon.ico is public", "/favicon.ico", nil, http.StatusOK, true},
		{"apple touch icon is public", "/apple-touch-icon.png", nil, http.StatusOK, true},
		{"cover is gated", "/api/archives/1/cover", nil, http.StatusUnauthorized, false},
		{"page is gated", "/api/archives/1/page/0", nil, http.StatusUnauthorized, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(testKey)
			var reached bool
			r := httptest.NewRequest("GET", tc.path, nil)
			if tc.set != nil {
				tc.set(r)
			}
			rec := httptest.NewRecorder()
			guarded(a, &reached).ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d", rec.Code, tc.want)
			}
			if reached != tc.passed {
				t.Fatalf("handler reached = %v, want %v", reached, tc.passed)
			}
		})
	}
}

func TestLoginSetsUsableCookie(t *testing.T) {
	a := testApp(testKey)
	rec := httptest.NewRecorder()
	a.handleAuthLogin(rec, httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"key":"`+testKey+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login code = %d, want 200", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("expected one %s cookie, got %v", sessionCookie, cookies)
	}
	if !cookies[0].HttpOnly {
		t.Error("session cookie should be HttpOnly")
	}
	if cookies[0].Secure {
		t.Error("session cookie should not be Secure over plain HTTP — the browser would drop it")
	}
	if cookies[0].SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookies[0].SameSite)
	}
	// A stolen cookie must not hand over the key itself.
	if v := cookies[0].Value; strings.Contains(v, testKey) ||
		strings.Contains(v, base64.RawURLEncoding.EncodeToString([]byte(testKey))) {
		t.Fatalf("cookie value %q carries the key", v)
	}
	var reached bool
	r := httptest.NewRequest("GET", "/api/archives", nil)
	r.AddCookie(cookies[0])
	rec2 := httptest.NewRecorder()
	guarded(a, &reached).ServeHTTP(rec2, r)
	if !reached {
		t.Fatalf("cookie from login did not authenticate: code %d", rec2.Code)
	}
}

func TestLoginRejectsAndTrims(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"correct key", `{"key":"` + testKey + `"}`, http.StatusOK},
		{"surrounding whitespace tolerated", `{"key":"  ` + testKey + `  "}`, http.StatusOK},
		{"wrong key", `{"key":"wrong"}`, http.StatusUnauthorized},
		{"empty key", `{"key":""}`, http.StatusUnauthorized},
		{"missing field", `{}`, http.StatusUnauthorized},
		{"not json", `nope`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(testKey)
			rec := httptest.NewRecorder()
			a.handleAuthLogin(rec, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestLoginThrottlesGuessing(t *testing.T) {
	a := testApp(testKey)
	post := func(key string) int {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"key":"`+key+`"}`))
		r.RemoteAddr = "192.0.2.7:1234"
		a.handleAuthLogin(rec, r)
		return rec.Code
	}
	for i := 0; i < loginMaxFails; i++ {
		if code := post("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: code = %d, want 401", i, code)
		}
	}
	if code := post("wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: code = %d, want 429", loginMaxFails, code)
	}
	// Blocked hard enough that the right key doesn't get through either.
	if code := post(testKey); code != http.StatusTooManyRequests {
		t.Fatalf("correct key while throttled: code = %d, want 429", code)
	}
	// A different client is unaffected.
	rec := httptest.NewRecorder()
	other := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"key":"`+testKey+`"}`))
	other.RemoteAddr = "192.0.2.8:1234"
	a.handleAuthLogin(rec, other)
	if rec.Code != http.StatusOK {
		t.Fatalf("unrelated client: code = %d, want 200", rec.Code)
	}
}

func TestAuthStatus(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		key                     string
		hdr                     string
		required, authenticated bool
	}{
		{"disabled", "", "", false, true},
		{"enabled, no key sent", testKey, "", true, false},
		{"enabled, key sent", testKey, testKey, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(tc.key)
			r := httptest.NewRequest("GET", "/api/auth/status", nil)
			if tc.hdr != "" {
				r.Header.Set("X-API-Key", tc.hdr)
			}
			rec := httptest.NewRecorder()
			a.handleAuthStatus(rec, r)
			body := rec.Body.String()
			wantReq := `"required":` + boolStr(tc.required)
			wantAuth := `"authenticated":` + boolStr(tc.authenticated)
			if !strings.Contains(body, wantReq) || !strings.Contains(body, wantAuth) {
				t.Fatalf("body = %s, want %s and %s", body, wantReq, wantAuth)
			}
		})
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	a := testApp(testKey)
	rec := httptest.NewRecorder()
	a.handleAuthLogout(rec, httptest.NewRequest("POST", "/api/auth/logout", nil))
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 || cookies[0].Value != "" {
		t.Fatalf("logout should expire the cookie, got %v", cookies)
	}
}

func TestCacheScope(t *testing.T) {
	if got := testApp("").cacheScope(); got != "public" {
		t.Errorf("no key: cacheScope = %q, want public", got)
	}
	if got := testApp(testKey).cacheScope(); got != "private" {
		t.Errorf("with key: cacheScope = %q, want private", got)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// The browser fetches icons without credentials, including on the login page,
// so they must come back through the real router even with a key configured.
func TestIconsServedWithoutKey(t *testing.T) {
	h := testApp(testKey).routes()
	for path, ctype := range map[string]string{
		"/favicon.svg":          "image/svg+xml",
		"/favicon.ico":          "image/x-icon",
		"/apple-touch-icon.png": "image/png",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ctype || rec.Body.Len() == 0 {
			t.Errorf("%s: code=%d type=%q len=%d, want 200 %q non-empty",
				path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len(), ctype)
		}
	}
}

func TestSessionToken(t *testing.T) {
	a := testApp(testKey)
	now := time.Now()
	tok := a.newSession(now)
	if !a.checkSession(tok, now) {
		t.Fatal("fresh token rejected")
	}
	if a.checkSession(tok, now.Add(sessionMaxAge+time.Second)) {
		t.Error("expired token accepted")
	}
	if testApp("a-different-key").checkSession(tok, now) {
		t.Error("token survived a key rotation")
	}
	// Pushing the expiry out without re-signing must not work.
	exp, sig, _ := strings.Cut(strings.TrimPrefix(tok, sessionTokenPrefix), ".")
	n, _ := strconv.ParseInt(exp, 10, 64)
	if a.checkSession(sessionTokenPrefix+strconv.FormatInt(n+1e6, 10)+"."+sig, now) {
		t.Error("token with edited expiry accepted")
	}
	for _, bad := range []string{"", "v1.", "v1.123", "v1.abc.def", tok[:len(tok)-2], "v2" + tok[2:]} {
		if a.checkSession(bad, now) {
			t.Errorf("malformed token %q accepted", bad)
		}
	}
}

// Guessing through X-API-Key on any endpoint must be throttled like the login
// form, or the login limit is meaningless.
func TestHeaderGuessesAreThrottled(t *testing.T) {
	a := testApp(testKey)
	var reached bool
	h := guarded(a, &reached)
	get := func(key string) int {
		r := httptest.NewRequest("GET", "/api/archives", nil)
		r.RemoteAddr = "192.0.2.9:1234"
		r.Header.Set("X-API-Key", key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for i := 0; i < loginMaxFails; i++ {
		if code := get("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("guess %d: code = %d, want 401", i, code)
		}
	}
	if code := get(testKey); code != http.StatusTooManyRequests {
		t.Fatalf("correct key after %d bad guesses: code = %d, want 429", loginMaxFails, code)
	}
	// The block is shared with the login form.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"key":"`+testKey+`"}`))
	r.RemoteAddr = "192.0.2.9:5678"
	a.handleAuthLogin(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("login after header guesses: code = %d, want 429", rec.Code)
	}
}

// The public status probe must not be a side door for guessing either.
func TestStatusProbeGuessesAreThrottled(t *testing.T) {
	a := testApp(testKey)
	probe := func(key string) string {
		r := httptest.NewRequest("GET", "/api/auth/status", nil)
		r.RemoteAddr = "192.0.2.11:1234"
		r.Header.Set("X-API-Key", key)
		rec := httptest.NewRecorder()
		a.handleAuthStatus(rec, r)
		return rec.Body.String()
	}
	for i := 0; i < loginMaxFails; i++ {
		probe("wrong")
	}
	if body := probe(testKey); !strings.Contains(body, `"authenticated":false`) {
		t.Fatalf("correct key accepted after %d bad guesses: %s", loginMaxFails, body)
	}
}

// A browser with a stale cookie fires a burst of failing image requests; that
// must not lock its owner out of logging in again.
func TestStaleCookiesDoNotThrottle(t *testing.T) {
	a := testApp(testKey)
	var reached bool
	h := guarded(a, &reached)
	stale := testApp("old-key").newSession(time.Now())
	for i := 0; i < loginMaxFails*3; i++ {
		r := httptest.NewRequest("GET", "/api/archives/1/cover", nil)
		r.RemoteAddr = "192.0.2.10:1234"
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: stale})
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"key":"`+testKey+`"}`))
	r.RemoteAddr = "192.0.2.10:1234"
	a.handleAuthLogin(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after stale-cookie burst: code = %d, want 200", rec.Code)
	}
}

func TestCrossOriginWritesRefused(t *testing.T) {
	tok := testApp(testKey).newSession(time.Now())
	tests := []struct {
		name   string
		method string
		hdr    map[string]string
		want   int
	}{
		{"same-origin POST", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"cross-site POST", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		// Another port on the same host is same-site, so Lax alone lets the cookie through.
		{"same-site PUT", "PUT", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cross-site GET is a read", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK},
		{"matching Origin, no fetch metadata", "POST", map[string]string{"Origin": "http://shelf.test"}, http.StatusOK},
		{"foreign Origin, no fetch metadata", "DELETE", map[string]string{"Origin": "http://nas.test:8081"}, http.StatusForbidden},
		{"proxy-forwarded host", "POST",
			map[string]string{"Origin": "https://books.example", "X-Forwarded-Host": "books.example"}, http.StatusOK},
		{"no browser headers (curl)", "POST", nil, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(testKey)
			var reached bool
			r := httptest.NewRequest(tc.method, "http://shelf.test/api/scan", nil)
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
			for k, v := range tc.hdr {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			guarded(a, &reached).ServeHTTP(rec, r)
			if rec.Code != tc.want || reached != (tc.want == http.StatusOK) {
				t.Fatalf("code = %d reached = %v, want %d", rec.Code, reached, tc.want)
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	var reached bool
	rec := httptest.NewRecorder()
	guarded(testApp(testKey), &reached).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	for k, v := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestClientIPBucketsIPv6By64(t *testing.T) {
	ip := func(addr string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = addr
		return clientIP(r)
	}
	if a, b := ip("[2001:db8:1:2::1]:1"), ip("[2001:db8:1:2:ffff::9]:2"); a != b {
		t.Errorf("same /64 bucketed apart: %q vs %q", a, b)
	}
	if a, b := ip("[2001:db8:1:2::1]:1"), ip("[2001:db8:1:3::1]:1"); a == b {
		t.Errorf("different /64s share a bucket: %q", a)
	}
	if got := ip("192.0.2.1:80"); got != "192.0.2.1" {
		t.Errorf("IPv4 = %q, want 192.0.2.1", got)
	}
}

func TestLoginLimiterIsBounded(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < loginMaxIPs*2; i++ {
		l.fail("10.0." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256))
	}
	if n := len(l.fails); n > loginMaxIPs {
		t.Fatalf("limiter holds %d entries, cap is %d", n, loginMaxIPs)
	}
}
