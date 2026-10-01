package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func secured() http.Handler {
	return security(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/signed-out" {
			page(w, 200, "Signed out", "ok", "", "")
			return
		}
		jsonResponse(w, 200, map[string]string{"ok": "1"})
	}))
}

func do(method, path string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	for i := 0; i < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	secured().ServeHTTP(w, r)
	return w
}

func TestCSRF(t *testing.T) {
	const js = "application/json"
	tests := []struct {
		name    string
		method  string
		headers []string
		want    int
	}{
		{"json no origin (curl)", "POST", []string{"Content-Type", js}, 200},
		{"json with charset", "POST", []string{"Content-Type", js + "; charset=utf-8"}, 200},
		{"same origin", "POST", []string{"Content-Type", js, "Origin", "http://example.com"}, 200},
		{"cross origin", "POST", []string{"Content-Type", js, "Origin", "http://evil.test"}, 403},
		{"origin null", "DELETE", []string{"Content-Type", js, "Origin", "null"}, 403},
		{"scheme mismatch", "POST", []string{"Content-Type", js, "Origin", "https://example.com"}, 403},
		{"gzip with custom header", "POST", []string{"Content-Type", "application/gzip", "X-Requested-With", "money-tracker"}, 200},
		{"gzip without header", "POST", []string{"Content-Type", "application/gzip"}, 415},
		{"gzip with header, cross origin", "POST", []string{"Content-Type", "application/gzip", "X-Requested-With", "money-tracker", "Origin", "http://evil.test"}, 403},
		{"wrong custom header value", "POST", []string{"Content-Type", "text/plain", "X-Requested-With", "x"}, 415},
		{"text/plain", "POST", []string{"Content-Type", "text/plain"}, 415},
		{"form", "POST", []string{"Content-Type", "application/x-www-form-urlencoded"}, 415},
		{"no content type", "PATCH", nil, 415},
		{"cross origin form", "POST", []string{"Content-Type", "application/x-www-form-urlencoded", "Origin", "http://evil.test"}, 403},
		{"GET unaffected", "GET", []string{"Origin", "http://evil.test", "Sec-Fetch-Site", "cross-site"}, 200},
		{"HEAD unaffected", "HEAD", []string{"Origin", "http://evil.test"}, 200},
	}
	for _, tc := range tests {
		if got := do(tc.method, "/api/x", tc.headers...).Code; got != tc.want {
			t.Errorf("%s: HTTP %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestHTTPSOriginAndHSTS(t *testing.T) {
	if do("GET", "/api/x").Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent over plain http config")
	}
	// X-Forwarded-Proto must not turn HSTS on.
	if do("GET", "/api/x", "X-Forwarded-Proto", "https").Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS trusted X-Forwarded-Proto")
	}
	t.Setenv("OIDC_REDIRECT_URL", "https://example.com/auth/callback")
	if do("GET", "/api/x").Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS missing with https config")
	}
	if got := do("POST", "/auth/logout", "Content-Type", "application/json", "Origin", "https://example.com").Code; got != 200 {
		t.Errorf("https same origin: %d", got)
	}
	if got := do("POST", "/auth/logout", "Content-Type", "application/json", "Origin", "http://example.com").Code; got != 403 {
		t.Errorf("http origin on https deployment: %d", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	for _, path := range []string{"/api/x", "/auth/signed-out", "/"} {
		h := do("GET", path).Header()
		for k, want := range map[string]string{
			"Content-Security-Policy": "frame-ancestors 'none'",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
		} {
			if !strings.Contains(h.Get(k), want) {
				t.Errorf("%s: %s = %q", path, k, h.Get(k))
			}
		}
		if strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self' 'unsafe") || strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self';") == false {
			t.Errorf("%s: weak CSP", path)
		}
		if noStore := h.Get("Cache-Control") == "no-store"; noStore != strings.HasPrefix(path, "/api/") {
			t.Errorf("%s: Cache-Control = %q", path, h.Get("Cache-Control"))
		}
	}
	// Headers are also present on rejected requests.
	if do("POST", "/api/x").Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("headers missing on 415")
	}
}

func TestAPIErrorDoesNotLeak(t *testing.T) {
	w := httptest.NewRecorder()
	apiError(w, errors.New(`pq: password authentication failed for user "secret-user"`))
	if w.Code < 500 || strings.Contains(w.Body.String(), "secret-user") || strings.Contains(w.Body.String(), "pq:") {
		t.Fatalf("HTTP %d leaks: %s", w.Code, w.Body.String())
	}
}
