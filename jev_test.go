package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testJevKey = "jev-secret-key-123"

// fakeJev records every request body and answers each question by looking at the sent name.
type fakeJev struct {
	mu     sync.Mutex
	bodies [][]byte
	auth   []string
	status int                                       // 0 = 200
	answer func(kind, name string) (string, float64) // choice, confidence; "" = leave the question unanswered
	server *httptest.Server
}

func newFakeJev(t *testing.T, answer func(kind, name string) (string, float64)) *fakeJev {
	f := &fakeJev{answer: answer}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, buf.Bytes())
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		status := f.status
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, "boom "+testJevKey, status)
			return
		}
		var req struct {
			State     map[string]json.RawMessage `json:"state"`
			Questions map[string]jevQuestion     `json:"questions"`
		}
		if err := json.Unmarshal(buf.Bytes(), &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		items := map[string]map[string]map[string]any{} // kind (merchants, accounts) -> question id -> fields; household is context only
		for kind, raw := range req.State {
			if kind == "household" {
				continue
			}
			var m map[string]map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Errorf("state %s: %v", kind, err)
			}
			items[kind] = m
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			for kind, byID := range items {
				name, _ := byID[id]["name"].(string)
				choice, conf := f.answer(kind, name)
				if choice == "" {
					continue
				}
				probs := map[string]float64{}
				for k := range q.Criteria {
					probs[k] = 0
				}
				probs[choice] = 1
				answers[id] = map[string]any{"type": "choice", "choice": choice, "confidence": conf, "probabilities": probs}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-latest", "answers": answers,
			"usage": map[string]int{"input_tokens": 100 * len(req.Questions), "output_tokens": 5 * len(req.Questions)}})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeJev) client() *jevClient {
	return &jevClient{key: testJevKey, url: f.server.URL, model: "jev-latest", minConfidence: 0.5, http: &http.Client{Timeout: 5 * time.Second}}
}

func (f *fakeJev) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func captureLog(t *testing.T) *bytes.Buffer {
	buf := new(bytes.Buffer)
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

func TestJevSuggestionsShapeMappingThresholdAndLocalFirst(t *testing.T) {
	fake := newFakeJev(t, func(_, name string) (string, float64) {
		switch name {
		case "Acme Widgets":
			return "Shopping", 0.9
		case "Foo Bar":
			return "Dining", 0.3 // below the default 0.5
		}
		return "", 0
	})
	a := categoryApp(t)
	a.jev = fake.client()
	a.jev.usage, a.jev.priceIn, a.jev.priceOut = a.db, 2, 10 // $ per million tokens (overriding the defaults)
	a.addTx(t, "k1", "KING SOOPERS #1", -1000)               // local keyword source answers this
	a.addTx(t, "j1", "ACME WIDGETS 55", -2500)
	a.addTx(t, "j2", "ACME WIDGETS 56", -3500)
	a.addTx(t, "f1", "FOO BAR", -100)
	a.addTx(t, "z1", "ZZZ UNKNOWN LLC", -100) // Jev has no answer

	var list []map[string]any
	a.call(t, "GET", "/api/suggestions", nil, 200, &list)
	got := map[string]map[string]any{}
	for _, s := range list {
		got[s["merchant"].(string)] = s
	}
	if s := got["king soopers"]; s == nil || s["source"] != "keyword" {
		t.Errorf("king soopers = %v", s)
	}
	acme := got["acme widgets"]
	if acme == nil || acme["category"] != "Shopping" || acme["source"] != "jev" || acme["confidence"] != 0.9 {
		t.Fatalf("acme = %v", acme)
	}
	if p, _ := acme["probabilities"].(map[string]any); p["Shopping"] != 1.0 {
		t.Errorf("probabilities not mapped: %v", acme["probabilities"])
	}
	if s := got["foo bar"]; s == nil || s["category"] != "Dining" || s["reason"] != "Jev's best guess, low confidence" {
		t.Errorf("low-confidence Jev answer should be a marked guess: %v", s)
	}
	if _, ok := got["zzz unknown llc"]; ok {
		t.Error("unanswered merchant was suggested")
	}

	if fake.requests() != 1 {
		t.Fatalf("want one batched request, got %d", fake.requests())
	}
	if fake.auth[0] != "Bearer "+testJevKey {
		t.Errorf("auth header = %q", fake.auth[0])
	}
	body := string(fake.bodies[0])
	if strings.Contains(body, "KING SOOPERS") || strings.Contains(strings.ToLower(body), "king soopers") {
		t.Errorf("Jev received a merchant the local sources answered: %s", body)
	}
	if strings.Contains(body, testJevKey) {
		t.Error("key in request body")
	}
	var req struct {
		Model     string                    `json:"model"`
		State     map[string]map[string]any `json:"state"`
		Questions map[string]jevQuestion    `json:"questions"`
	}
	if err := json.Unmarshal(fake.bodies[0], &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "jev-latest" || len(req.Questions) != 3 || len(req.State) != 1 || len(req.State["merchants"]) != 3 {
		t.Fatalf("request shape: %s", body)
	}
	for id, q := range req.Questions {
		if q.Type != "choice" || q.Instructions == "" || len(q.Criteria) != len(defaultCategories) {
			t.Errorf("question %s = %+v", id, q)
		}
		for _, c := range defaultCategories {
			if q.Criteria[c] != categoryDescriptions[c] {
				t.Errorf("criteria[%s] = %q", c, q.Criteria[c])
			}
		}
	}
	// Only these fields per merchant; the amount is the typical one (-25.00 for acme's two rows... total -6000/2 = -30.00).
	allowed := map[string]bool{"name": true, "bank_descriptions": true, "typical_amount": true, "kind": true, "charges": true, "typical_days_between_charges": true}
	for id, m := range req.State["merchants"] {
		for k := range m.(map[string]any) {
			if !allowed[k] {
				t.Errorf("merchant %s sends disallowed field %q", id, k)
			}
		}
		if mm := m.(map[string]any); mm["name"] == "Acme Widgets" && (mm["typical_amount"] != -30.0 || mm["kind"] != "spending") {
			t.Errorf("acme = %v", m)
		}
	}
	for _, banned := range []string{"account", "balance", "\"id\"", "date", "k1", "sf:"} {
		if strings.Contains(strings.ToLower(body), banned) {
			t.Errorf("request body mentions %q: %s", banned, body)
		}
	}

	// Usage: the one request (3 questions) is recorded, with the estimated cost.
	var cfg struct {
		Usage map[string]map[string]float64 `json:"jev_usage"`
	}
	a.call(t, "GET", "/api/config", nil, 200, &cfg)
	if u := cfg.Usage["last_30_days"]; u["requests"] != 1 || u["input_tokens"] != 300 || u["output_tokens"] != 15 || u["cost"] != (300*2+15*10)/1e6 {
		t.Errorf("jev usage = %v", cfg.Usage)
	}

	// Saved: the next load asks Jev nothing, not even about merchants it had no confident answer for.
	a.call(t, "GET", "/api/suggestions", nil, 200, &list)
	if fake.requests() != 1 || len(list) != 3 {
		t.Fatalf("second load: %d Jev requests, suggestions %v", fake.requests(), list)
	}
	// Asking again forgets the saved answers, and the user's own categories are offered by name.
	if _, err := a.db.Exec(`UPDATE transactions SET user_category='Gardening' WHERE name='KING SOOPERS #1'`); err != nil {
		t.Fatal(err)
	}
	a.jev.home = "Denver, Colorado"
	var cleared map[string]int
	a.call(t, "DELETE", "/api/suggestions/jev", nil, 200, &cleared)
	if cleared["cleared"] != 3 {
		t.Errorf("cleared = %v", cleared)
	}
	a.call(t, "GET", "/api/suggestions", nil, 200, &list)
	if fake.requests() != 2 {
		t.Fatalf("after asking again: %d Jev requests", fake.requests())
	}
	if err := json.Unmarshal(fake.bodies[1], &req); err != nil {
		t.Fatal(err)
	}
	for id, q := range req.Questions {
		if q.Criteria["Gardening"] != "Gardening" || q.Criteria["Dining"] != categoryDescriptions["Dining"] {
			t.Errorf("question %s criteria = %v", id, q.Criteria)
		}
	}
	// Household context: the home location and the user's own categorized merchants, names and categories only.
	var ctxBody struct {
		State struct {
			Household map[string]json.RawMessage `json:"household"`
			Merchants map[string]map[string]any  `json:"merchants"`
		} `json:"state"`
	}
	if err := json.Unmarshal(fake.bodies[1], &ctxBody); err != nil {
		t.Fatal(err)
	}
	h := ctxBody.State.Household
	if string(h["home"]) != `"Denver, Colorado"` || string(h["examples"]) != `[{"category":"Gardening","merchant":"King Soopers"}]` || len(h) != 2 {
		t.Errorf("household = %s", fake.bodies[1])
	}
	for id, m := range ctxBody.State.Merchants {
		if m["name"] == "Acme Widgets" && m["charges"] != 2.0 {
			t.Errorf("merchant %s = %v", id, m)
		}
	}
}

func TestJevIncomeKindAndMinConfidenceEnv(t *testing.T) {
	fake := newFakeJev(t, func(_, name string) (string, float64) { return "Income", 0.6 })
	a := categoryApp(t)
	a.jev = fake.client()
	a.addTx(t, "p1", "ACME PAYROLL", 300000)
	if s := a.suggestions(t)["acme payroll"]; s.Source != "jev" || s.Category != "Income" {
		t.Errorf("payroll = %+v", s)
	}
	if !strings.Contains(string(fake.bodies[0]), `"kind":"income"`) {
		t.Errorf("kind not income: %s", fake.bodies[0])
	}
	t.Setenv("JEV_API_KEY", "k")
	t.Setenv("JEV_MIN_CONFIDENCE", "0.8")
	if j := newJevFromEnv(); j.minConfidence != 0.8 || j.model != "jev-latest" || j.url != jevDefaultURL {
		t.Errorf("env config = %+v", j)
	}
	t.Setenv("JEV_MIN_CONFIDENCE", "junk")
	if j := newJevFromEnv(); j.minConfidence != 0.5 {
		t.Errorf("default threshold = %v", j.minConfidence)
	}
	t.Setenv("JEV_API_KEY", "")
	if newJevFromEnv() != nil {
		t.Error("Jev enabled without a key")
	}
}

func TestJevErrorAndTimeoutFallThrough(t *testing.T) {
	logs := captureLog(t)
	fake := newFakeJev(t, func(_, _ string) (string, float64) { return "Shopping", 0.9 })
	fake.status = 500
	a := categoryApp(t)
	a.jev = fake.client()
	a.addTx(t, "k1", "KING SOOPERS #1", -1000)
	a.addTx(t, "j1", "ACME WIDGETS", -2500)
	got := a.suggestions(t)
	if got["king soopers"].Source != "keyword" {
		t.Errorf("local suggestions broken by a Jev error: %+v", got)
	}
	if _, ok := got["acme widgets"]; ok {
		t.Error("suggestion from a failed Jev call")
	}
	if !strings.Contains(logs.String(), "suggestion source jev: jev returned HTTP 500") {
		t.Errorf("log = %q", logs.String())
	}

	// Timeout, with the key embedded in the URL so a leaky error message would show it.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	a.jev = &jevClient{key: testJevKey, url: slow.URL + "/?k=" + testJevKey, model: "jev-latest", minConfidence: 0.5, http: &http.Client{Timeout: 50 * time.Millisecond}}
	if got := a.suggestions(t); got["king soopers"].Source != "keyword" {
		t.Errorf("timeout broke local suggestions: %+v", got)
	}
	if !strings.Contains(logs.String(), "suggestion source jev: jev request failed") || !strings.Contains(logs.String(), "[redacted]") {
		t.Errorf("log = %q", logs.String())
	}
	if strings.Contains(logs.String(), testJevKey) {
		t.Errorf("API key leaked into logs: %q", logs.String())
	}
	_, err := jevSource{a.jev, a.db}.Suggest(context.Background(), []MerchantSample{{Key: "x", Display: "X"}}, categoryChoices(defaultCategories))
	if err == nil || strings.Contains(err.Error(), testJevKey) {
		t.Errorf("error = %v", err)
	}
}

func (a *app) acct(t *testing.T, id string) (guess string, confident bool, user string) {
	t.Helper()
	if err := a.db.QueryRow(`SELECT guessed_type,guess_confident,user_type FROM accounts WHERE id=$1`, id).Scan(&guess, &confident, &user); err != nil {
		t.Fatal(err)
	}
	return
}

func TestJevAccountTypes(t *testing.T) {
	logs := captureLog(t)
	fake := newFakeJev(t, func(_, name string) (string, float64) {
		switch name {
		case "Acct 9":
			return "investment", 0.9
		case "Acct 7":
			return "loan", 0.2
		case "Acct 9 renamed":
			return "credit", 0.8
		}
		return "", 0
	})
	a := categoryApp(t)
	a.jev = fake.client()
	if _, err := a.db.Exec(`DELETE FROM accounts; INSERT INTO accounts(id,item_id,institution,name,current,guessed_type,guess_confident,user_type) VALUES
		('sf:mystery','item','Test Bank','Acct 9',500,'other',false,''),
		('sf:low','item','Test Bank','Acct 7',-500,'credit',false,''),
		('sf:visa','item','Test Bank','Sapphire',-100,'credit',true,''),
		('sf:mine','item','Test Bank','Acct 5',100,'other',false,'depository')`); err != nil {
		t.Fatal(err)
	}
	a.jevAccountTypes(context.Background())
	if fake.requests() != 1 {
		t.Fatalf("want one batched request, got %d", fake.requests())
	}
	body := string(fake.bodies[0])
	for _, banned := range []string{"Sapphire", "Acct 5", "sf:", "500", "100", "Bearer", testJevKey} {
		if strings.Contains(body, banned) {
			t.Errorf("request contains %q: %s", banned, body)
		}
	}
	for _, want := range []string{`"institution":"Test Bank"`, `"balance_sign":"positive"`, `"balance_sign":"negative"`, `"name":"Acct 9"`} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %s: %s", want, body)
		}
	}
	if g, c, u := a.acct(t, "sf:mystery"); g != "investment" || !c || u != "" {
		t.Errorf("mystery = %q %v %q", g, c, u)
	}
	if g, c, _ := a.acct(t, "sf:low"); g != "credit" || c {
		t.Errorf("below threshold must stay unconfident: %q %v", g, c)
	}
	if g, c, u := a.acct(t, "sf:mine"); g != "other" || c || u != "depository" {
		t.Errorf("user-set account touched: %q %v %q", g, c, u)
	}
	// The effective type is the Jev guess, and the API reports it as a guess, not a user choice.
	if got := a.acctState(t, "sf:mystery"); got != (acctState{"investment", "guess", true}) {
		t.Errorf("api state = %+v", got)
	}

	a.jevAccountTypes(context.Background()) // same names: nothing to ask
	if fake.requests() != 1 {
		t.Errorf("re-asked with unchanged names: %d requests", fake.requests())
	}
	// A user override always wins, then clearing it brings the Jev guess back.
	a.call(t, "PATCH", "/api/accounts/sf:mystery", map[string]string{"type": "loan"}, 200, nil)
	if got := a.acctState(t, "sf:mystery"); got != (acctState{"loan", "user", true}) {
		t.Errorf("override = %+v", got)
	}
	a.call(t, "PATCH", "/api/accounts/sf:mystery", map[string]string{"type": ""}, 200, nil)
	if got := a.acctState(t, "sf:mystery"); got != (acctState{"investment", "guess", true}) {
		t.Errorf("after clearing = %+v", got)
	}

	// Rename: asked again, alone.
	a.db.Exec(`UPDATE accounts SET name='Acct 9 renamed',guessed_type='other',guess_confident=false WHERE id='sf:mystery'`)
	a.jevAccountTypes(context.Background())
	if fake.requests() != 2 || strings.Contains(string(fake.bodies[1]), "Acct 7") {
		t.Fatalf("rename re-ask: %d requests, %s", fake.requests(), fake.bodies[len(fake.bodies)-1])
	}
	if g, c, _ := a.acct(t, "sf:mystery"); g != "credit" || !c {
		t.Errorf("after rename = %q %v", g, c)
	}
	// A user type set after Jev answered is never changed by a later Jev answer.
	a.db.Exec(`UPDATE accounts SET user_type='depository',name='Acct 9' WHERE id='sf:mystery'`)
	a.jevAccountTypes(context.Background())
	if _, _, u := a.acct(t, "sf:mystery"); u != "depository" || fake.requests() != 2 {
		t.Errorf("user_type = %q, requests %d", u, fake.requests())
	}
	if strings.Contains(logs.String(), testJevKey) {
		t.Error("key in logs")
	}
}

func TestSyncSucceedsWhenJevFailsAndKeepsJevGuess(t *testing.T) {
	logs := captureLog(t)
	sfin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"accounts":[{"org":{"name":"Test Bank"},"id":"mystery","name":"Acct 9","balance":"5.00"}]}`)
	}))
	defer sfin.Close()
	fake := newFakeJev(t, func(_, _ string) (string, float64) { return "investment", 0.9 })
	fake.status = 503
	a := categoryApp(t)
	a.db.Exec(`DELETE FROM accounts`)
	a.jev = fake.client()
	a.simplefin = simplefinClient{client: sfin.Client(), accessURL: strings.Replace(sfin.URL, "https://", "https://user:pass@", 1)}
	if err := a.ensureSimplefinItem(); err != nil {
		t.Fatal(err)
	}
	sync := func() {
		t.Helper()
		if err := a.syncSimplefin(context.Background()); err != nil {
			t.Fatalf("sync failed: %v", err)
		}
	}
	sync()
	if g, c, _ := a.acct(t, "sf:mystery"); g != "other" || c {
		t.Errorf("after failed Jev = %q %v", g, c)
	}
	if !strings.Contains(logs.String(), "jev account types: jev returned HTTP 503") {
		t.Errorf("log = %q", logs.String())
	}
	if fake.requests() != 1 {
		t.Fatalf("requests = %d", fake.requests())
	}
	fake.mu.Lock()
	fake.status = 0
	fake.mu.Unlock()
	sync() // failures are not recorded as asked, so this retries and succeeds
	if g, c, _ := a.acct(t, "sf:mystery"); g != "investment" || !c {
		t.Errorf("after Jev answer = %q %v", g, c)
	}
	sync() // the sync's own local guess must not wipe the Jev answer, and it is not re-asked
	if g, c, _ := a.acct(t, "sf:mystery"); g != "investment" || !c || fake.requests() != 2 {
		t.Errorf("after re-sync = %q %v, %d requests", g, c, fake.requests())
	}
	if strings.Contains(logs.String(), testJevKey) {
		t.Error("key in logs")
	}
}

func TestConfigJevEnabled(t *testing.T) {
	a := categoryApp(t)
	for _, on := range []bool{false, true} {
		if on {
			a.jev = &jevClient{key: testJevKey}
		}
		var cfg map[string]any
		a.call(t, "GET", "/api/config", nil, 200, &cfg)
		if cfg["jev_enabled"] != on {
			t.Errorf("jev_enabled = %v, want %v", cfg["jev_enabled"], on)
		}
		if strings.Contains(fmt.Sprint(cfg), testJevKey) {
			t.Error("key in config")
		}
	}
}
