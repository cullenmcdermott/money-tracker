package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDecimalCents(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		bad   bool
	}{
		{"-12.3", -1230, false},
		{"1234.56", 123456, false},
		{"0.5", 50, false},
		{"100", 10000, false},
		{"+1.20", 120, false},
		{"1.2300", 123, false},
		{"-0.01", -1, false},
		{"92233720368547758.07", 9223372036854775807, false},
		{"", 0, true},
		{"1.234", 0, true},
		{"1.2x", 0, true},
		{".5", 0, true},
		{"1.", 0, true},
		{"92233720368547758.08", 0, true},
	}
	for _, test := range tests {
		got, err := decimalCents(test.input)
		if (err != nil) != test.bad || (!test.bad && got != test.want) {
			t.Errorf("decimalCents(%q) = %d, %v; want %d, bad=%v", test.input, got, err, test.want, test.bad)
		}
	}
}

func TestSimplefinRejectsHTTPRedirect(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/accounts", nil)
	if err := (simplefinClient{}).httpClient().CheckRedirect(req, nil); err == nil {
		t.Fatal("HTTP redirect accepted")
	}
}

func TestSimplefinClaimAndSync(t *testing.T) {
	now := time.Now().UTC()
	posted := now.Add(-24 * time.Hour).Unix()
	var windows [][2]int64
	var filters []string // the account= filter of each request, "" for all accounts
	failSync := false
	progress := &syncProgress{}
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simplefin/accounts":
			if failSync {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			user, password, ok := r.BasicAuth()
			if !ok || user != "user" || password != "pass" {
				t.Errorf("Basic auth = %q, %q, %v", user, password, ok)
			}
			if r.URL.User != nil || r.URL.Query().Get("pending") != "1" || r.URL.Query().Get("version") != "2" {
				t.Errorf("accounts URL = %s", r.URL.Redacted())
			}
			start, err := strconv.ParseInt(r.URL.Query().Get("start-date"), 10, 64)
			end, endErr := strconv.ParseInt(r.URL.Query().Get("end-date"), 10, 64)
			if err != nil || endErr != nil || end-start > 45*24*60*60 || end <= start || start < now.Add(-89*24*time.Hour).Unix() {
				t.Errorf("start-date = %d, %v", start, err)
			}
			windows = append(windows, [2]int64{start, end})
			filters = append(filters, strings.Join(r.URL.Query()["account"], ","))
			if p := progress.get(); !p.Running || p.Stage == "" || p.StartedAt.IsZero() {
				t.Errorf("progress during request = %+v", p)
			}
			balance := "100.25"
			extra := fmt.Sprintf(`,{"id":"older","posted":%d,"amount":"-5.00"}`, now.Add(-70*24*time.Hour).Unix())
			fmt.Fprintf(w, `{"errors":["One account needs attention"],"accounts":[{"org":{"name":"Test Bank","domain":"example.test"},"id":"checking","name":"Checking","currency":"USD","balance":"%s","available-balance":"90.00","balance-date":%d,"transactions":[{"id":"same","posted":%d,"amount":"-2.50","description":"Coffee","payee":"Cafe"},{"id":"pending","posted":0,"transacted_at":%d,"amount":"-1.00","description":"Hold","pending":true},{"id":"bad-date","amount":"1.00"},{"id":"bad-amount","posted":%d,"amount":"oops"}%s]},{"org":{"name":"Card Bank"},"id":"credit","name":"Credit card","currency":"USD","balance":"-20.50","balance-date":%d,"transactions":[{"id":"same","posted":%d,"amount":"-4.00","description":"Charge"}]},{"id":"broken","name":"Broken","balance":"oops","transactions":[{"id":"valid","posted":%d,"amount":"3.00"}]}]}`, balance, posted, posted, posted, posted, extra, posted, posted, posted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := app{db: db, simplefin: simplefinClient{client: server.Client(), accessURL: strings.Replace(server.URL, "https://", "https://user:pass@", 1) + "/simplefin", progress: progress}}
	if err := a.ensureSimplefinItem(); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureSimplefinItem(); err != nil { // idempotent
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if response.Code != 200 {
		t.Fatalf("first sync: HTTP %d %s", response.Code, response.Body.String())
	}
	if len(windows) != 2 || windows[0][1] != windows[1][0] || windows[0][0] > now.Add(-89*24*time.Hour).Unix()+60 {
		t.Errorf("first sync windows = %v", windows)
	}
	response = httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sync", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"running":false`) {
		t.Errorf("progress after sync: HTTP %d %s", response.Code, response.Body.String())
	}
	var warning, synced string
	if err := db.QueryRow(`SELECT last_error,last_synced_at FROM items WHERE id=$1`, simplefinItemID).Scan(&warning, &synced); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, "One account needs attention") || !strings.Contains(warning, "skipped transaction checking/bad-date: no date") || !strings.Contains(warning, "skipped transaction checking/bad-amount:") || !strings.Contains(warning, "skipped balance broken:") || synced == "" {
		t.Errorf("item status = %q, %q", warning, synced)
	}
	if strings.Count(warning, "One account needs attention") != 1 {
		t.Errorf("duplicate warning: %q", warning)
	}
	var current, available sql.NullInt64
	if err := db.QueryRow(`SELECT current,available FROM accounts WHERE id='sf:broken'`).Scan(&current, &available); err != nil || current.Valid || available.Valid {
		t.Errorf("bad balance = %v, %v, %v", current, available, err)
	}
	var badBalanceCount, goodTransactionCount, badTransactionCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM balances WHERE account_id='sf:broken'`).Scan(&badBalanceCount)
	_ = db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE id='sf:broken:valid'`).Scan(&goodTransactionCount)
	_ = db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE id IN ('sf:checking:bad-date','sf:checking:bad-amount')`).Scan(&badTransactionCount)
	if badBalanceCount != 0 || goodTransactionCount != 1 || badTransactionCount != 0 {
		t.Errorf("bad data counts = %d balances, %d valid transactions, %d invalid transactions", badBalanceCount, goodTransactionCount, badTransactionCount)
	}
	var olderCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE id='sf:checking:older'`).Scan(&olderCount); err != nil || olderCount != 1 {
		t.Errorf("older window transaction count = %d, %v", olderCount, err)
	}
	for _, account := range []struct {
		id, institution string
		current         int64
	}{
		{"sf:checking", "Test Bank", 10025},
		{"sf:credit", "Card Bank", -2050},
	} {
		var institution string
		var current, balance int64
		if err := db.QueryRow(`SELECT institution,current FROM accounts WHERE id=$1`, account.id).Scan(&institution, &current); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT current FROM balances WHERE account_id=$1 AND date=$2`, account.id, now.Format("2006-01-02")).Scan(&balance); err != nil {
			t.Fatal(err)
		}
		if institution != account.institution || current != account.current || balance != current {
			t.Errorf("account %s = %q, %d, %d", account.id, institution, current, balance)
		}
	}
	var date, merchant string
	var amount int64
	var pending bool
	if err := db.QueryRow(`SELECT date,amount,merchant,pending FROM transactions WHERE id='sf:checking:pending'`).Scan(&date, &amount, &merchant, &pending); err != nil {
		t.Fatal(err)
	}
	if date != time.Unix(posted, 0).UTC().Format("2006-01-02") || amount != -100 || merchant != "" || !pending {
		t.Errorf("pending transaction = %q, %d, %q, %v", date, amount, merchant, pending)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE id IN ('sf:checking:same','sf:credit:same')`).Scan(&count); err != nil || count != 2 {
		t.Errorf("account-scoped transaction IDs: count %d, %v", count, err)
	}
	response = httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if response.Code != 200 {
		t.Fatalf("sync: HTTP %d %s", response.Code, response.Body.String())
	}
	if len(windows) != 3 || filters[2] != "" || windows[2][0] < now.Add(-14*24*time.Hour).Unix()-60 || windows[2][0] > now.Add(-14*24*time.Hour).Unix()+60 {
		t.Errorf("repeat sync should read from 14 days before the last one: %v", windows)
	}
	if _, err := db.Exec(`UPDATE items SET last_synced_at=$1 WHERE id=$2`, now.Add(-200*24*time.Hour).Format(time.RFC3339), simplefinItemID); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if response.Code != 200 || len(windows) != 5 {
		t.Errorf("stale sync: HTTP %d, windows %v", response.Code, windows)
	}
	response = httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/items", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Test Bank") || strings.Contains(response.Body.String(), "user:pass") {
		t.Errorf("items: HTTP %d %s", response.Code, response.Body.String())
	}
	// An account added in Bridge since the last sync: the recent read plus only its older history, in 45-day windows.
	if _, err := db.Exec(`DELETE FROM accounts WHERE id='sf:credit'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE items SET last_synced_at=$1 WHERE id=$2`, now.Format(time.RFC3339), simplefinItemID); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if response.Code != 200 || len(windows) != 8 || filters[5] != "" || filters[6] != "credit" || filters[7] != "credit" || windows[7][1] != windows[5][0] {
		t.Errorf("new account backfill: HTTP %d, windows %v, filters %q", response.Code, windows, filters)
	}
	var creditBalance int64
	if err := db.QueryRow(`SELECT current FROM accounts WHERE id='sf:credit'`).Scan(&creditBalance); err != nil || creditBalance != -2050 {
		t.Errorf("backfilled account balance = %d, %v", creditBalance, err)
	}
	failSync = true
	if _, err := db.Exec(`UPDATE items SET last_synced_at=$1 WHERE id=$2`, now.Add(-200*24*time.Hour).Format(time.RFC3339), simplefinItemID); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	var lastError string
	_ = db.QueryRow(`SELECT last_error FROM items WHERE id=$1`, simplefinItemID).Scan(&lastError)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "HTTP 503") || !strings.Contains(lastError, "HTTP 503") || strings.Contains(response.Body.String()+lastError, "pass") {
		t.Errorf("failed sync: HTTP %d %s, last_error %q", response.Code, response.Body.String(), lastError)
	}
	var spent int
	if err := db.QueryRow(`SELECT COUNT(*) FROM simplefin_requests`).Scan(&spent); err != nil || spent != 9 {
		t.Errorf("tracked requests = %d, %v; want 9 (eight windows and the failed one)", spent, err)
	}
	if _, err := db.Exec(`INSERT INTO simplefin_requests(at) SELECT now() - interval '25 hours' UNION ALL SELECT now() FROM generate_series(1, 15)`); err != nil {
		t.Fatal(err)
	}
	failSync = false
	response = httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "limit of 24 requests") || len(windows) != 8 {
		t.Errorf("sync over budget: HTTP %d %s, windows %v", response.Code, response.Body.String(), windows)
	}
}

const testAccessURL = "https://user:s3cret@bridge.example.test/simplefin"

func TestSimplefinFromEnv(t *testing.T) {
	for _, test := range []struct {
		raw        string
		configured bool
		bad        bool
	}{
		{testAccessURL, true, false},
		{"", false, false},
		{"  ", false, false},
		{"http://user:s3cret@bridge.example.test/simplefin", false, true},
		{"https://bridge.example.test/simplefin", false, true},
		{"https://user@bridge.example.test/simplefin", false, true},
		{"not a url s3cret", false, true},
		{"https://user:s3cret@bad host/%zz", false, true},
		{"://s3cret", false, true},
	} {
		got, err := simplefinFromEnv(test.raw)
		if (err != nil) != test.bad || (got.accessURL != "") != test.configured {
			t.Errorf("simplefinFromEnv(%q) = %q, %v", test.raw, got.accessURL, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error leaks credentials: %v", err)
		}
	}
}

func TestSimplefinRedactsErrors(t *testing.T) {
	// A dead server makes the request fail; nothing that comes back may carry the credentials.
	server := httptest.NewTLSServer(http.NotFoundHandler())
	server.Close()
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := app{db: db, simplefin: simplefinClient{client: server.Client(), accessURL: strings.Replace(server.URL, "https://", "https://user:s3cret@", 1)}}
	if err := a.ensureSimplefinItem(); err != nil {
		t.Fatal(err)
	}
	err = a.syncSimplefin(context.Background())
	var stored string
	_ = db.QueryRow(`SELECT last_error FROM items WHERE id=$1`, simplefinItemID).Scan(&stored)
	if err == nil || stored == "" || strings.Contains(err.Error()+stored, "s3cret") {
		t.Errorf("sync error = %v, stored %q", err, stored)
	}
	if got := a.simplefin.redact("failed GET " + a.simplefin.accessURL + " password s3cret"); strings.Contains(got, "s3cret") {
		t.Errorf("redact left the password: %q", got)
	}
}

func TestSimplefinUnconfigured(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := app{db: db}
	var config struct {
		Configured bool `json:"simplefin_configured"`
	}
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil || response.Code != 200 || config.Configured {
		t.Errorf("config: HTTP %d %s, %v", response.Code, response.Body.String(), err)
	}
	response = httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/items", nil))
	if response.Code != 200 || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Errorf("items: HTTP %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "SIMPLEFIN_ACCESS_URL") {
		t.Errorf("sync: HTTP %d %s", response.Code, response.Body.String())
	}
}

func TestClaimRoutesAreGone(t *testing.T) {
	a := app{}
	for _, r := range []struct{ method, path string }{{"POST", "/api/simplefin/claim"}, {"POST", "/api/link/token"}, {"POST", "/api/link/exchange"}, {"POST", "/api/sandbox/seed"}, {"DELETE", "/api/items/simplefin"}} {
		response := httptest.NewRecorder()
		a.routes().ServeHTTP(response, httptest.NewRequest(r.method, r.path, strings.NewReader("{}")))
		if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: HTTP %d", r.method, r.path, response.Code)
		}
	}
}

func claimServer(t *testing.T, status int, body string) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.ContentLength > 0 {
			t.Errorf("claim request: %s, length %d", r.Method, r.ContentLength)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server, base64.StdEncoding.EncodeToString([]byte(server.URL + "/claim"))
}

func TestSimplefinClaimCommand(t *testing.T) {
	server, token := claimServer(t, 200, testAccessURL+"\n")
	var stdout, stderr bytes.Buffer
	if code := runSimplefinClaim(strings.NewReader("  "+token+"\n"), &stdout, &stderr, simplefinClient{client: server.Client()}); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if stdout.String() != testAccessURL+"\n" {
		t.Errorf("stdout = %q, want only the access URL", stdout.String())
	}
	if strings.Contains(stderr.String(), "s3cret") {
		t.Errorf("stderr leaks credentials: %q", stderr.String())
	}
}

func TestSimplefinClaimFailures(t *testing.T) {
	forbidden, forbiddenToken := claimServer(t, 403, "user:s3cret")
	badURL, badURLToken := claimServer(t, 200, "http://user:s3cret@example.test/simplefin")
	for name, test := range map[string]struct {
		input  string
		client simplefinClient
	}{
		"bad base64":      {"!!!not-base64!!!", simplefinClient{}},
		"empty":           {"", simplefinClient{}},
		"http claim URL":  {base64.StdEncoding.EncodeToString([]byte("http://bridge.example.test/claim")), simplefinClient{}},
		"credentials URL": {base64.StdEncoding.EncodeToString([]byte("https://user:s3cret@bridge.example.test/claim")), simplefinClient{}},
		"rejected":        {forbiddenToken, simplefinClient{client: forbidden.Client()}},
		"bad access URL":  {badURLToken, simplefinClient{client: badURL.Client()}},
		"unreachable":     {base64.StdEncoding.EncodeToString([]byte("https://127.0.0.1:1/claim")), simplefinClient{}},
	} {
		var stdout, stderr bytes.Buffer
		if code := runSimplefinClaim(strings.NewReader(test.input), &stdout, &stderr, test.client); code == 0 || stdout.Len() != 0 || strings.Contains(stderr.String(), "s3cret") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, stdout.String(), stderr.String())
		}
	}
}

func TestSyncDue(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check := func(name string, want bool, setup string) {
		t.Helper()
		if _, err := db.Exec(`DELETE FROM simplefin_requests; DELETE FROM items; ` + setup); err != nil {
			t.Fatal(err)
		}
		if due, err := syncDue(ctx, db); err != nil || due != want {
			t.Errorf("%s: due=%v err=%v, want %v", name, due, err, want)
		}
	}
	check("never synced", true, `SELECT 1`)
	check("synced 2h ago", false, `INSERT INTO items(id,last_synced_at) VALUES('simplefin', to_char(now() - interval '2 hours','YYYY-MM-DD"T"HH24:MI:SSOF'))`)
	check("synced a day ago", true, `INSERT INTO items(id,last_synced_at) VALUES('simplefin', to_char(now() - interval '25 hours','YYYY-MM-DD"T"HH24:MI:SSOF'))`)
	check("failed attempt an hour ago", false, `INSERT INTO items(id,last_synced_at) VALUES('simplefin', to_char(now() - interval '25 hours','YYYY-MM-DD"T"HH24:MI:SSOF'));
		INSERT INTO simplefin_requests(at) VALUES(now() - interval '1 hour')`)
}
