package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeIdP is an in-process OIDC provider: discovery, JWKS, and a token endpoint that verifies PKCE
// and signs ID tokens with a throwaway RSA key.
type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	codes map[string]fakeGrant
}

type fakeGrant struct {
	challenge string
	claims    map[string]any
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, codes: map[string]fakeGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "end_session_endpoint": f.srv.URL + "/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		g, ok := f.codes[r.FormValue("code")]
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if !ok || b64(sum[:]) != g.challenge { // PKCE S256 check
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		claims := map[string]any{"iss": f.srv.URL, "aud": "cid", "sub": "user-1", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
		for k, v := range g.claims {
			claims[k] = v
		}
		head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
		body, _ := json.Marshal(claims)
		signing := b64(head) + "." + b64(body)
		sum = sha256.Sum256([]byte(signing))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": signing + "." + b64(sig)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// grant plays the provider's side of /authorize for the redirect the app sent, returning the callback code.
func (f *fakeIdP) grant(loc *url.URL, claims map[string]any) string {
	q := loc.Query()
	c := map[string]any{"nonce": q.Get("nonce"), "email": "me@example.com", "name": "Me"}
	for k, v := range claims {
		c[k] = v
	}
	code := "code-" + q.Get("state")
	f.codes[code] = fakeGrant{challenge: q.Get("code_challenge"), claims: c}
	return code
}

type browser struct {
	h       http.Handler
	cookies map[string]*http.Cookie
}

func (b *browser) do(method, target string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	for _, c := range b.cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return rec
}

type testEnv struct {
	idp *fakeIdP
	a   *authn
	b   *browser
}

func setup(t *testing.T, override map[string]string) *testEnv {
	t.Helper()
	idp := newFakeIdP(t)
	env := map[string]string{
		"OIDC_ISSUER": idp.srv.URL, "OIDC_CLIENT_ID": "cid", "OIDC_REDIRECT_URL": "http://localhost:8080/auth/callback",
		"SESSION_SECRET": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), "OIDC_ALLOWED_EMAILS": "Me@Example.com",
	}
	for k, v := range override {
		env[k] = v
	}
	a, err := newAuth(context.Background(), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	next := http.NewServeMux()
	next.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("app")) })
	next.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return &testEnv{idp: idp, a: a, b: &browser{h: a.handler(next), cookies: map[string]*http.Cookie{}}}
}

// login runs /auth/login then /auth/callback and returns the callback response.
func (e *testEnv) login(t *testing.T, ret string, claims map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := e.b.do("GET", "/auth/login?return="+url.QueryEscape(ret))
	loc, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != 302 || err != nil {
		t.Fatalf("login: %d %v", rec.Code, err)
	}
	code := e.idp.grant(loc, claims)
	return e.b.do("GET", "/auth/callback?code="+code+"&state="+loc.Query().Get("state"))
}

func TestLoginRoundTrip(t *testing.T) {
	e := setup(t, nil)
	// Unauthenticated page load bounces to login, remembering the path.
	rec := e.b.do("GET", "/transactions?month=2026-09")
	if rec.Code != 302 || rec.Header().Get("Location") != "/auth/login?return=%2Ftransactions%3Fmonth%3D2026-09" {
		t.Fatalf("page redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = e.b.do("GET", rec.Header().Get("Location"))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" ||
		q.Get("response_type") != "code" || q.Get("scope") != "openid email profile groups" || q.Get("client_id") != "cid" {
		t.Fatalf("authorize params: %v", q)
	}
	lc := e.b.cookies[loginCookie]
	if lc == nil || !lc.HttpOnly || lc.SameSite != http.SameSiteLaxMode || lc.Path != "/auth" {
		t.Fatalf("login cookie: %+v", lc)
	}
	code := e.idp.grant(loc, nil)
	rec = e.b.do("GET", "/auth/callback?code="+code+"&state="+q.Get("state"))
	if rec.Code != 302 || rec.Header().Get("Location") != "/transactions?month=2026-09" {
		t.Fatalf("callback: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	sc := e.b.cookies[sessionCookie]
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" || sc.Secure || sc.MaxAge != int(sessionTTL.Seconds()) {
		t.Fatalf("session cookie: %+v", sc)
	}
	if e.b.cookies[loginCookie] != nil {
		t.Fatal("login cookie should be cleared")
	}
	rec = e.b.do("GET", "/api/me")
	var me map[string]any
	json.Unmarshal(rec.Body.Bytes(), &me)
	if rec.Code != 200 || me["email"] != "me@example.com" || me["name"] != "Me" {
		t.Fatalf("/api/me: %d %s", rec.Code, rec.Body)
	}
	if rec = e.b.do("GET", "/"); rec.Body.String() != "app" {
		t.Fatalf("app not served: %d %s", rec.Code, rec.Body)
	}
	// Logout clears the cookie and points at the provider's end_session_endpoint.
	rec = e.b.do("POST", "/auth/logout", "Content-Type", "application/json")
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out["redirect"], e.idp.srv.URL+"/logout?") || !strings.Contains(out["redirect"], "client_id=cid") {
		t.Fatalf("logout redirect: %v", out)
	}
	if e.b.do("GET", "/api/me").Code != 401 {
		t.Fatal("session survived logout")
	}
}

func TestSecureCookieOnHTTPS(t *testing.T) {
	e := setup(t, map[string]string{"OIDC_REDIRECT_URL": "https://money.example.com/auth/callback"})
	if rec := e.login(t, "/", nil); rec.Code != 302 || !e.b.cookies[sessionCookie].Secure {
		t.Fatalf("expected Secure session cookie, got %d %+v", rec.Code, e.b.cookies[sessionCookie])
	}
}

func TestCallbackRejections(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		state  string // override
		want   int
	}{
		{"state mismatch", nil, "forged", 400},
		{"nonce mismatch", map[string]any{"nonce": "other"}, "", 401},
		{"expired id token", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, "", 401},
		{"wrong audience", map[string]any{"aud": "someone-else"}, "", 401},
		{"wrong issuer", map[string]any{"iss": "https://evil.example"}, "", 401},
		{"email not allowed", map[string]any{"email": "eve@example.com"}, "", 403},
		{"no email, no group", map[string]any{"email": "", "groups": []string{"other"}}, "", 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, nil)
			rec := e.b.do("GET", "/auth/login")
			loc, _ := url.Parse(rec.Header().Get("Location"))
			state := loc.Query().Get("state")
			code := e.idp.grant(loc, c.claims)
			if c.state != "" {
				state = c.state
			}
			rec = e.b.do("GET", "/auth/callback?code="+code+"&state="+state)
			if rec.Code != c.want || e.b.cookies[sessionCookie] != nil {
				t.Fatalf("got %d (session set: %v), want %d", rec.Code, e.b.cookies[sessionCookie] != nil, c.want)
			}
		})
	}
}

func TestCallbackWithoutLoginCookie(t *testing.T) {
	e := setup(t, nil)
	if rec := e.b.do("GET", "/auth/callback?code=x&state=y"); rec.Code != 400 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestGroupAllowList(t *testing.T) {
	e := setup(t, map[string]string{"OIDC_ALLOWED_EMAILS": "", "OIDC_ALLOWED_GROUPS": "money, admins"})
	if rec := e.login(t, "/", map[string]any{"email": "x@y.z", "groups": []string{"admins"}}); rec.Code != 302 {
		t.Fatalf("group member denied: %d", rec.Code)
	}
	e = setup(t, map[string]string{"OIDC_ALLOWED_EMAILS": "", "OIDC_ALLOWED_GROUPS": "money"})
	if rec := e.login(t, "/", map[string]any{"groups": []string{"other"}}); rec.Code != 403 {
		t.Fatalf("non-member allowed: %d", rec.Code)
	}
}

func TestTamperedAndExpiredSession(t *testing.T) {
	e := setup(t, nil)
	e.login(t, "/", nil)
	good := e.b.cookies[sessionCookie].Value
	if e.b.do("GET", "/api/me").Code != 200 {
		t.Fatal("valid session rejected")
	}
	for name, v := range map[string]string{
		"flipped byte": good[:len(good)-3] + map[bool]string{true: "AAA", false: "BBB"}[!strings.HasSuffix(good, "AAA")],
		"truncated":    good[:10],
		"garbage":      "not-a-cookie!!",
	} {
		e.b.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: v}
		if e.b.do("GET", "/api/me").Code != 401 {
			t.Errorf("%s: accepted", name)
		}
	}
	// A login-cookie blob must not be usable as a session (cookie name is GCM AAD).
	e.b.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: e.a.seal(loginCookie, session{Email: "me@example.com", Exp: time.Now().Add(time.Hour).Unix()})}
	if e.b.do("GET", "/api/me").Code != 401 {
		t.Error("cross-cookie blob accepted")
	}
	// Expiry.
	e.b.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: good}
	e.a.now = func() time.Time { return time.Now().Add(sessionTTL + time.Minute) }
	if e.b.do("GET", "/api/me").Code != 401 {
		t.Error("expired session accepted")
	}
}

func TestOpenRedirectRejected(t *testing.T) {
	for _, bad := range []string{"https://evil.example", "//evil.example", "/\\evil.example", "/\t/evil.example", "javascript:alert(1)", "evil", "/auth/login", ""} {
		if got := safeReturn(bad); got != "/" {
			t.Errorf("safeReturn(%q) = %q", bad, got)
		}
	}
	if got := safeReturn("/accounts?x=1#a"); got != "/accounts?x=1#a" {
		t.Errorf("valid path mangled: %q", got)
	}
	// End to end: a hostile return value lands on "/".
	e := setup(t, nil)
	if rec := e.login(t, "https://evil.example/", nil); rec.Header().Get("Location") != "/" {
		t.Fatalf("redirected to %q", rec.Header().Get("Location"))
	}
}

func TestUnauthenticatedAccess(t *testing.T) {
	e := setup(t, nil)
	rec := e.b.do("GET", "/api/transactions")
	if rec.Code != 401 || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("api: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec = e.b.do("POST", "/api/sync", "Content-Type", "application/json"); rec.Code != 401 {
		t.Fatalf("api POST: %d", rec.Code)
	}
	if rec = e.b.do("GET", "/api/me"); rec.Code != 401 {
		t.Fatalf("/api/me: %d", rec.Code)
	}
	if rec = e.b.do("GET", "/api/health"); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("health must be public: %d", rec.Code)
	}
	if rec = e.b.do("GET", "/assets/app.js"); rec.Code != 302 {
		t.Fatalf("static asset: %d", rec.Code)
	}
}

func TestStartupRefusesWithoutConfig(t *testing.T) {
	good := map[string]string{
		"OIDC_ISSUER": "http://127.0.0.1:1", "OIDC_CLIENT_ID": "cid", "OIDC_REDIRECT_URL": "https://m.example.com/auth/callback",
		"SESSION_SECRET": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), "OIDC_ALLOWED_EMAILS": "me@example.com",
	}
	with := func(k, v string) func(string) string {
		return func(key string) string {
			if key == k {
				return v
			}
			return good[key]
		}
	}
	cases := map[string]struct {
		get  func(string) string
		want string
	}{
		"nothing set":         {func(string) string { return "" }, "AUTH_DISABLED=true"},
		"no allow rule":       {with("OIDC_ALLOWED_EMAILS", ""), "OIDC_ALLOWED_EMAILS or OIDC_ALLOWED_GROUPS"},
		"no secret":           {with("SESSION_SECRET", ""), "SESSION_SECRET"},
		"short secret":        {with("SESSION_SECRET", base64.StdEncoding.EncodeToString([]byte("short"))), "32 bytes"},
		"http redirect":       {with("OIDC_REDIRECT_URL", "http://m.example.com/auth/callback"), "https"},
		"wrong redirect path": {with("OIDC_REDIRECT_URL", "https://m.example.com/cb"), "/auth/callback"},
		"auth disabled typo":  {with("AUTH_DISABLED", "yes"), ""}, // only "true" disables; falls through to discovery failure
	}
	for name, c := range cases {
		a, err := newAuth(context.Background(), c.get)
		if err == nil || a != nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

func TestAuthDisabled(t *testing.T) {
	dev := map[string]string{"AUTH_DISABLED": "true", "ADDR": "127.0.0.1:8188"}
	for name, env := range map[string]map[string]string{
		"default addr": {"AUTH_DISABLED": "true"},
		"public addr":  {"AUTH_DISABLED": "true", "ADDR": ":8080"},
		"with oidc":    {"AUTH_DISABLED": "true", "ADDR": "127.0.0.1:8188", "OIDC_ISSUER": "https://id.example.com"},
	} {
		if _, err := newAuth(context.Background(), func(k string) string { return env[k] }); err == nil {
			t.Errorf("%s: AUTH_DISABLED accepted", name)
		}
	}
	a, err := newAuth(context.Background(), func(k string) string { return dev[k] })
	if err != nil {
		t.Fatal(err)
	}
	b := &browser{h: a.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("app")) })), cookies: map[string]*http.Cookie{}}
	for _, u := range []string{"http://localhost:5188/api/anything", "http://127.0.0.1:8188/api/anything", "http://[::1]:8188/api/anything"} {
		if rec := b.do("GET", u); rec.Body.String() != "app" {
			t.Fatalf("%s: got %d %s", u, rec.Code, rec.Body)
		}
	}
	if rec := b.do("GET", "http://localhost:5188/api/me"); !strings.Contains(rec.Body.String(), `"auth":false`) {
		t.Fatalf("got %s", rec.Body)
	}
	// DNS rebinding: the attacker's name resolves to 127.0.0.1 but the Host header keeps it.
	if rec := b.do("GET", "http://evil.example:8188/api/accounts"); rec.Code != http.StatusForbidden {
		t.Fatalf("rebinding host served: %d", rec.Code)
	}
}
