package main

// In-app OIDC login (Pocket ID or any standards-compliant provider) for the single user.
// Authorization code flow + PKCE (S256) + state + nonce. No server-side state: the session is an
// AES-GCM encrypted cookie, so nothing touches the database.
//
// Session lifetime is a FIXED 30 days (no sliding refresh): allow-list changes and revocation take
// effect within 30 days at most, and rotating SESSION_SECRET invalidates every session at once.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "mt_session"
	loginCookie   = "mt_login"
	sessionTTL    = 30 * 24 * time.Hour
	loginTTL      = 10 * time.Minute
)

type authn struct {
	disabled   bool
	oauth      oauth2.Config
	verifier   *oidc.IDTokenVerifier
	gcm        cipher.AEAD
	secure     bool
	base       string // scheme://host of the redirect URL
	endSession string // provider's end_session_endpoint, if advertised
	emails     map[string]bool
	groups     map[string]bool
	now        func() time.Time
}

type session struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Exp   int64  `json:"exp"`
}

type loginState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Return   string `json:"return"`
	Exp      int64  `json:"exp"`
}

// mustAuth wraps next with authentication, or exits with a clear message when auth is misconfigured.
func mustAuth(ctx context.Context, next http.Handler) http.Handler {
	a, err := newAuth(ctx, os.Getenv)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	return a.handler(next)
}

func csvSet(s string, lower bool) map[string]bool {
	set := map[string]bool{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			if lower {
				v = strings.ToLower(v)
			}
			set[v] = true
		}
	}
	return set
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// hostname strips the port from a Host header ("localhost:5188", "[::1]:8188").
func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func newAuth(ctx context.Context, getenv func(string) string) (*authn, error) {
	if getenv("AUTH_DISABLED") == "true" {
		// Guard against it reaching a real deployment: it only works on a loopback ADDR and with no OIDC config.
		if host, _, err := net.SplitHostPort(getenv("ADDR")); err != nil || !loopback(host) {
			return nil, errors.New("AUTH_DISABLED=true needs ADDR on a loopback address such as 127.0.0.1:8188")
		}
		if getenv("OIDC_ISSUER") != "" {
			return nil, errors.New("AUTH_DISABLED=true cannot be combined with OIDC_ISSUER")
		}
		log.Print("WARNING: AUTH_DISABLED=true, every request is served WITHOUT authentication. Never use this outside local development.")
		return &authn{disabled: true}, nil
	}
	var missing []string
	for _, k := range []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_REDIRECT_URL", "SESSION_SECRET"} {
		if getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	emails, groups := csvSet(getenv("OIDC_ALLOWED_EMAILS"), true), csvSet(getenv("OIDC_ALLOWED_GROUPS"), false)
	if len(emails)+len(groups) == 0 {
		missing = append(missing, "OIDC_ALLOWED_EMAILS or OIDC_ALLOWED_GROUPS")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("authentication is not configured (missing %s). Set the OIDC_* variables and SESSION_SECRET, or set AUTH_DISABLED=true for local development only", strings.Join(missing, ", "))
	}
	secret, err := base64.StdEncoding.DecodeString(getenv("SESSION_SECRET"))
	if err != nil {
		if secret, err = base64.RawStdEncoding.DecodeString(getenv("SESSION_SECRET")); err != nil {
			return nil, errors.New("SESSION_SECRET must be base64 (generate one with: openssl rand -base64 32)")
		}
	}
	if len(secret) < 32 {
		return nil, errors.New("SESSION_SECRET must decode to at least 32 bytes (openssl rand -base64 32)")
	}
	redirect, err := url.Parse(getenv("OIDC_REDIRECT_URL"))
	if err != nil || redirect.Host == "" || (redirect.Scheme != "https" && redirect.Scheme != "http") {
		return nil, errors.New("OIDC_REDIRECT_URL must be an absolute URL such as https://money.example.com/auth/callback")
	}
	local := redirect.Hostname() == "localhost" || redirect.Hostname() == "127.0.0.1"
	if redirect.Scheme == "http" && !local {
		return nil, errors.New("OIDC_REDIRECT_URL must be https (plain http is only allowed for localhost)")
	}
	if redirect.Path != "/auth/callback" {
		return nil, errors.New("OIDC_REDIRECT_URL path must be /auth/callback")
	}
	// One key from SESSION_SECRET; the cookie name is bound in as GCM additional data.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("money-tracker/cookie-key/v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// The client is kept by go-oidc for JWKS refreshes, hence the timeout.
	ctx = oidc.ClientContext(ctx, &http.Client{Timeout: 15 * time.Second})
	provider, err := oidc.NewProvider(ctx, getenv("OIDC_ISSUER"))
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for %s failed: %w", getenv("OIDC_ISSUER"), err)
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = provider.Claims(&meta)
	return &authn{
		oauth: oauth2.Config{
			ClientID: getenv("OIDC_CLIENT_ID"), ClientSecret: getenv("OIDC_CLIENT_SECRET"), // secret optional: public client + PKCE
			Endpoint: provider.Endpoint(), RedirectURL: redirect.String(),
			Scopes: []string{oidc.ScopeOpenID, "email", "profile", "groups"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: getenv("OIDC_CLIENT_ID")}),
		gcm:      gcm, secure: redirect.Scheme == "https",
		base: redirect.Scheme + "://" + redirect.Host, endSession: meta.EndSession,
		emails: emails, groups: groups, now: time.Now,
	}, nil
}

func (a *authn) seal(name string, v any) string {
	b, _ := json.Marshal(v)
	nonce := make([]byte, a.gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(a.gcm.Seal(nonce, nonce, b, []byte(name)))
}

func (a *authn) open(r *http.Request, name string, v any) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(raw) < a.gcm.NonceSize() {
		return false
	}
	plain, err := a.gcm.Open(nil, raw[:a.gcm.NonceSize()], raw[a.gcm.NonceSize():], []byte(name))
	return err == nil && json.Unmarshal(plain, v) == nil
}

func (a *authn) setCookie(w http.ResponseWriter, name, value, path string, ttl time.Duration) {
	c := &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode}
	if ttl < 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(ttl.Seconds())
	}
	http.SetCookie(w, c)
}

func (a *authn) session(r *http.Request) (session, bool) {
	var s session
	return s, a.open(r, sessionCookie, &s) && a.now().Unix() < s.Exp
}

// safeReturn accepts only same-site relative paths ("/x?y#z"); anything else becomes "/".
func safeReturn(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/auth/") || strings.ContainsAny(p, `\`) {
		return "/"
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	if u, err := url.Parse(p); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}

func (a *authn) allowed(email string, groups []string) bool {
	if email != "" && a.emails[strings.ToLower(email)] {
		return true
	}
	for _, g := range groups {
		if a.groups[g] {
			return true
		}
	}
	return false
}

// page renders a bare-bones HTML message (no inline styles or scripts, so it works under a strict CSP).
func page(w http.ResponseWriter, status int, title, msg, linkHref, linkText string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	link := ""
	if linkHref != "" {
		link = fmt.Sprintf(`<p><a href="%s">%s</a></p>`, html.EscapeString(linkHref), html.EscapeString(linkText))
	}
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title></head><body><main><h1>%s</h1><p>%s</p>%s</main></body></html>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg), link)
}

func randToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *authn) login(w http.ResponseWriter, r *http.Request) {
	ls := loginState{State: randToken(), Nonce: randToken(), Verifier: oauth2.GenerateVerifier(), Return: safeReturn(r.URL.Query().Get("return")), Exp: a.now().Add(loginTTL).Unix()}
	a.setCookie(w, loginCookie, a.seal(loginCookie, ls), "/auth", loginTTL)
	http.Redirect(w, r, a.oauth.AuthCodeURL(ls.State, oauth2.S256ChallengeOption(ls.Verifier), oidc.Nonce(ls.Nonce)), http.StatusFound)
}

func (a *authn) callback(w http.ResponseWriter, r *http.Request) {
	var ls loginState
	ok := a.open(r, loginCookie, &ls) && a.now().Unix() < ls.Exp
	a.setCookie(w, loginCookie, "", "/auth", -1) // single use
	if !ok {
		page(w, http.StatusBadRequest, "Login expired", "The login attempt expired or did not start here. Please try again.", "/auth/login", "Sign in")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		page(w, http.StatusBadRequest, "Login failed", "The identity provider returned an error: "+e, "/auth/login", "Try again")
		return
	}
	if q.Get("code") == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(ls.State)) != 1 {
		page(w, http.StatusBadRequest, "Login failed", "The login state did not match. Please try again.", "/auth/login", "Try again")
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		log.Printf("auth: code exchange: %v", err)
		page(w, http.StatusBadGateway, "Login failed", "Could not complete sign-in with the identity provider.", "/auth/login", "Try again")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := a.verifier.Verify(r.Context(), raw) // issuer, audience, signature, expiry
	if err == nil && subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(ls.Nonce)) != 1 {
		err = errors.New("nonce mismatch")
	}
	var c struct {
		Email  string   `json:"email"`
		Name   string   `json:"name"`
		User   string   `json:"preferred_username"`
		Groups []string `json:"groups"`
	}
	if err == nil {
		err = idt.Claims(&c)
	}
	if err != nil {
		log.Printf("auth: id token: %v", err)
		page(w, http.StatusUnauthorized, "Login failed", "The identity provider's ID token could not be verified.", "/auth/login", "Try again")
		return
	}
	// email_verified is not required: this is a self-hosted provider where the admin sets emails. Use groups for stricter control.
	if !a.allowed(c.Email, c.Groups) {
		log.Printf("auth: denied %q (sub %s)", c.Email, idt.Subject)
		page(w, http.StatusForbidden, "Access denied", "Your account is not allowed to use this app. Ask the owner to add you to the allowed list.", "", "")
		return
	}
	if c.Name == "" {
		c.Name = c.User
	}
	a.setCookie(w, sessionCookie, a.seal(sessionCookie, session{Sub: idt.Subject, Email: c.Email, Name: c.Name, Exp: a.now().Add(sessionTTL).Unix()}), "/", sessionTTL)
	http.Redirect(w, r, safeReturn(ls.Return), http.StatusFound)
}

func (a *authn) logout(w http.ResponseWriter, r *http.Request) {
	a.setCookie(w, sessionCookie, "", "/", -1)
	next := "/auth/signed-out"
	if a.endSession != "" {
		// Needs <base>/auth/signed-out registered as a post-logout redirect URI at the provider.
		next = a.endSession + "?" + url.Values{"client_id": {a.oauth.ClientID}, "post_logout_redirect_uri": {a.base + "/auth/signed-out"}}.Encode()
	}
	jsonResponse(w, http.StatusOK, map[string]string{"redirect": next})
}

// handler serves /auth/* and /api/me, and gates everything else behind a valid session.
func (a *authn) handler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		if a.disabled {
			jsonResponse(w, http.StatusOK, map[string]any{"email": "", "name": "", "auth": false})
		} else if s, ok := a.session(r); ok {
			jsonResponse(w, http.StatusOK, map[string]any{"email": s.Email, "name": s.Name, "auth": true})
		} else {
			jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}
	})
	if !a.disabled {
		mux.HandleFunc("GET /auth/login", a.login)
		mux.HandleFunc("GET /auth/callback", a.callback)
		mux.HandleFunc("POST /auth/logout", a.logout)
		mux.HandleFunc("GET /auth/signed-out", func(w http.ResponseWriter, r *http.Request) {
			page(w, http.StatusOK, "Signed out", "You have been signed out.", "/auth/login", "Sign in again")
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// With auth off, only loopback Host names are served, so a DNS-rebinding page can't reach the dev server.
		if a.disabled && !loopback(hostname(r.Host)) {
			jsonResponse(w, http.StatusForbidden, map[string]string{"error": "auth is disabled: only localhost is served"})
			return
		}
		if a.disabled || r.URL.Path == "/api/health" { // health stays public for k8s probes
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.session(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		// The SPA shell and assets are protected too; an unauthenticated page load just bounces to login.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/api/") {
			http.Redirect(w, r, "/auth/login?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
	})
	return mux
}
