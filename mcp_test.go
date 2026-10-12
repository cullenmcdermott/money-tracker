package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The MCP server signs in through the real auth handler's /mcp/login, then reads /api/* with the read-only token.
func TestMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	e := setup(t, nil)
	srv := httptest.NewServer(e.b.h)
	t.Cleanup(srv.Close)

	fresh := &browser{h: e.b.h, cookies: map[string]*http.Cookie{}}
	if rec := fresh.do("GET", "/mcp/login?port=1&state=s"); rec.Code != 302 || rec.Header().Get("Location") != "/auth/login?return=%2Fmcp%2Flogin%3Fport%3D1%26state%3Ds" {
		t.Fatalf("signed-out /mcp/login: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	e.login(t, "/", nil)
	if rec := e.b.do("GET", "/mcp/login?port=x&state=s"); rec.Code != 400 {
		t.Fatalf("bad port: %d", rec.Code)
	}

	browsed := 0
	t.Cleanup(func(u func(string)) func() { return func() { openBrowser = u } }(openBrowser))
	openBrowser = func(u string) { // the signed-in browser follows /mcp/login's redirect to the loopback listener
		browsed++
		loc, _ := url.Parse(u)
		rec := e.b.do("GET", loc.RequestURI())
		resp, err := http.Get(rec.Header().Get("Location"))
		if err != nil || resp.StatusCode != 200 {
			t.Errorf("callback: %v %v", err, resp)
		}
	}
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get","arguments":{"path":"/api/accounts"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get","arguments":{"path":"https://evil.example/api/x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
	}
	var out bytes.Buffer
	if code := runMCP([]string{srv.URL}, strings.NewReader(strings.Join(lines, "\n")), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var resps []map[string]any
	for dec := json.NewDecoder(&out); dec.More(); {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, r)
	}
	text := func(r map[string]any) string {
		return r["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	if len(resps) != 5 || resps[0]["result"].(map[string]any)["protocolVersion"] != "2025-06-18" ||
		!strings.Contains(text(resps[2]), "app") || resps[2]["result"].(map[string]any)["isError"] != false ||
		!strings.Contains(text(resps[3]), "must start with /api/") || resps[4]["error"] == nil || browsed != 1 {
		t.Fatalf("browsed %d, responses %v", browsed, resps)
	}

	// A second run reuses the cached token without the browser.
	out.Reset()
	runMCP([]string{srv.URL}, strings.NewReader(lines[3]), &out, io.Discard)
	if browsed != 1 || !strings.Contains(out.String(), `"isError":false`) {
		t.Fatalf("cached token not used: %d %s", browsed, out.String())
	}

	dir, _ := os.UserConfigDir()
	b, err := os.ReadFile(filepath.Join(dir, "money-tracker", "mcp-token.json"))
	var c struct{ Token string }
	if err != nil || json.Unmarshal(b, &c) != nil || c.Token == "" {
		t.Fatalf("token cache: %v %s", err, b)
	}
	anon := &browser{h: e.b.h, cookies: map[string]*http.Cookie{}}
	bearer := []string{"Authorization", "Bearer " + c.Token}
	if rec := anon.do("GET", "/api/accounts", bearer...); rec.Code != 200 {
		t.Fatalf("bearer GET: %d", rec.Code)
	}
	if rec := anon.do("POST", "/api/accounts", append(bearer, "Content-Type", "application/json")...); rec.Code != 401 {
		t.Fatalf("bearer must be read-only: %d", rec.Code)
	}
	if rec := anon.do("GET", "/", bearer...); rec.Code != 302 {
		t.Fatalf("bearer must only reach /api/: %d", rec.Code)
	}
	anon.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: c.Token}
	if rec := anon.do("GET", "/api/accounts"); rec.Code != 401 {
		t.Fatalf("token worked as a session cookie: %d", rec.Code)
	}
	e.a.now = func() time.Time { return time.Now().Add(sessionTTL + time.Hour) }
	if rec := (&browser{h: e.b.h, cookies: map[string]*http.Cookie{}}).do("GET", "/api/accounts", bearer...); rec.Code != 401 {
		t.Fatalf("expired token accepted: %d", rec.Code)
	}
}
