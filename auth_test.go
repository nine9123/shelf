package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	cookie := &http.Cookie{Name: sessionCookie, Value: encodeCookieValue(testKey)}
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
	// The key contains characters net/http strips from raw cookie values; the
	// round trip only survives because it's encoded.
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
