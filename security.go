package main

import (
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// The built SPA has no inline scripts or <style> elements, but its rendered HTML uses style="" attributes,
// so only style-src-attr allows 'unsafe-inline'. Google Fonts are the only external origin.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' https://fonts.googleapis.com; style-src-attr 'unsafe-inline'; font-src https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// httpsConfigured reports whether the deployment is served over https, from config (OIDC_REDIRECT_URL)
// rather than a client-controllable X-Forwarded-Proto header.
func httpsConfigured(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(os.Getenv("OIDC_REDIRECT_URL"), "https://")
}

// security wraps the whole app (including /auth/*) with security headers and CSRF checks.
func security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		if httpsConfigured(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !sameOrigin(r) {
				jsonResponse(w, http.StatusForbidden, map[string]string{"error": "cross-site request rejected"})
				return
			}
			// JSON needs a CORS preflight cross-site; so does a custom header (for raw uploads such as backup restore).
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if (err != nil || mt != "application/json") && r.Header.Get("X-Requested-With") != "money-tracker" {
				jsonResponse(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json (or send X-Requested-With: money-tracker)"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin allows a missing Origin (curl, non-browser clients) and otherwise requires scheme and host to match.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	scheme := "http"
	if httpsConfigured(r) {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && u.Host == r.Host
}
