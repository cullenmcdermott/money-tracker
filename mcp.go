package main

// `money-tracker mcp <url>`: a stdio MCP server that gives an LLM client read-only access to a running instance's
// JSON API. It signs in through the app's own OIDC login: on the first 401 it opens <url>/mcp/login in the browser,
// which sends a read-only token back to a listener on 127.0.0.1. The token (good for GET /api/* only, and for the
// rest of the browser session's 30 days) is cached in the user config dir.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const mcpGetDoc = `GET a path from the money-tracker JSON API. Read-only. Amounts are integer cents; negative = money out.
Periods (month, period) are YYYY-MM, YYYY or YYYY-MM..YYYY-MM.
- /api/accounts: every account with current balance and type
- /api/transactions?month=&q=&category=&account_id=&flow=in|out&uncategorized=1&limit=1-1000&offset=: newest first, 100 by default (X-Total-Count gives the total). Filtering by category or flow leaves out transfers and pending rows
- /api/summary/monthly: income, expense and invested per month
- /api/summary/categories?month=[&by=merchant]: spending per category (or merchant)
- /api/summary/income?month=: income per source
- /api/networth: daily net worth, assets and debts
- /api/categories, /api/rules, /api/merchants
- /api/recurring: subscriptions, bills and paychecks
- /api/alerts: unusual charges
- /api/investments?period=: growth split into contributions, dividends and market change; holdings and allocation
- /api/plan, /api/goals: retirement projection and savings goals
- /api/sync: sync status`

var openBrowser = func(u string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	_ = exec.Command(cmd, u).Start()
}

type mcpClient struct {
	base   *url.URL
	token  string
	cache  string // token file
	stderr io.Writer
}

func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		args = []string{""}
	}
	base, err := url.Parse(strings.TrimSuffix(args[0], "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		fmt.Fprintln(stderr, "usage: money-tracker mcp https://money.example.com")
		return 2
	}
	m := &mcpClient{base: base, stderr: stderr}
	if dir, err := os.UserConfigDir(); err == nil {
		m.cache = filepath.Join(dir, "money-tracker", "mcp-token.json")
		var c struct{ URL, Token string }
		if b, err := os.ReadFile(m.cache); err == nil && json.Unmarshal(b, &c) == nil && c.URL == base.String() {
			m.token = c.Token
		}
	}
	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 1<<20), 16<<20)
	out := json.NewEncoder(stdout)
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || req.ID == nil {
			continue // notifications need no reply
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if result, err := m.handle(req.Method, req.Params); err != nil {
			resp["error"] = map[string]any{"code": -32601, "message": err.Error()}
		} else {
			resp["result"] = result
		}
		if out.Encode(resp) != nil {
			return 1
		}
	}
	return 0
}

func (m *mcpClient) handle(method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p struct{ ProtocolVersion string }
		_ = json.Unmarshal(params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]string{"name": "money-tracker", "version": "1"}}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": []any{map[string]any{
			"name": "get", "description": mcpGetDoc, "annotations": map[string]bool{"readOnlyHint": true},
			"inputSchema": map[string]any{"type": "object", "required": []string{"path"},
				"properties": map[string]any{"path": map[string]string{"type": "string", "description": "e.g. /api/transactions?month=2026-09&q=coffee"}}},
		}}}, nil
	case "tools/call":
		var p struct {
			Name      string
			Arguments struct{ Path string }
		}
		_ = json.Unmarshal(params, &p)
		text, err := m.get(p.Arguments.Path)
		if err != nil {
			text = err.Error()
		}
		return map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": err != nil}, nil
	}
	return nil, fmt.Errorf("method not found: %s", method)
}

func (m *mcpClient) get(path string) (string, error) {
	p, err := url.Parse(path)
	if err != nil || p.Scheme != "" || p.Host != "" || !strings.HasPrefix(p.Path, "/api/") {
		return "", errors.New("path must start with /api/")
	}
	target := *m.base
	target.Path, target.RawQuery = m.base.Path+p.Path, p.RawQuery
	for retried := false; ; retried = true {
		req, _ := http.NewRequest(http.MethodGet, target.String(), nil)
		if m.token != "" {
			req.Header.Set("Authorization", "Bearer "+m.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		if resp.StatusCode == http.StatusUnauthorized && !retried {
			if err := m.login(); err != nil {
				return "", err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%s: %s", resp.Status, body)
		}
		if total := resp.Header.Get("X-Total-Count"); total != "" {
			return "X-Total-Count: " + total + "\n" + string(body), nil
		}
		return string(body), nil
	}
}

// login opens the app's /mcp/login in the browser and waits for the token on a one-off loopback listener.
func (m *mcpClient) login() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	state := randToken()
	got := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/callback" || q.Get("state") != state || q.Get("token") == "" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		select {
		case got <- q.Get("token"):
		default:
		}
		fmt.Fprintln(w, "Signed in to money-tracker. You can close this tab.")
	})}
	go srv.Serve(ln)
	defer srv.Close()
	login := m.base.String() + "/mcp/login?" + url.Values{"port": {strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)}, "state": {state}}.Encode()
	fmt.Fprintln(m.stderr, "money-tracker: sign in at", login)
	openBrowser(login)
	select {
	case m.token = <-got:
	case <-time.After(3 * time.Minute):
		return errors.New("sign-in timed out; open this in a browser and try again: " + login)
	}
	if m.cache != "" {
		b, _ := json.Marshal(map[string]string{"URL": m.base.String(), "Token": m.token})
		if err := os.MkdirAll(filepath.Dir(m.cache), 0o700); err == nil {
			_ = os.WriteFile(m.cache, b, 0o600)
		}
	}
	return nil
}
