package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestChargeState(t *testing.T) {
	for raw, want := range map[string]string{
		"SAFEWAY #1234 DENVER CO": "CO",
		"ADOBE SAN JOSE CA":       "CA",
		"NETFLIX.COM":             "",
		"CO":                      "",
		"STARBUCKS STORE 99":      "",
	} {
		if got := chargeState(raw); got != want {
			t.Errorf("chargeState(%q) = %q, want %q", raw, got, want)
		}
	}
}

// alertFixture builds a cash-flow history: a checking account and a card, both open since January, with a
// grocery store charged weekly at home (CO) on the card.
type alertFixture struct {
	txs   []alertTx
	first map[string]time.Time
	today time.Time
}

func newAlertFixture() *alertFixture {
	d := func(s string) time.Time { v, _ := time.Parse(time.DateOnly, s); return v }
	f := &alertFixture{first: map[string]time.Time{"card": d("2026-01-02"), "chk": d("2026-01-02")}, today: d("2026-09-20")}
	for day := d("2026-06-01"); day.Before(f.today); day = day.AddDate(0, 0, 7) {
		f.add("card", "grocer", "GROCER #12 DENVER CO", day.Format(time.DateOnly), 4000)
	}
	return f
}

func (f *alertFixture) add(account, merchant, desc, date string, cents int64) string {
	d, _ := time.Parse(time.DateOnly, date)
	id := fmt.Sprintf("t%d", len(f.txs))
	f.txs = append(f.txs, alertTx{id: id, account: account, merchant: merchant, name: merchant, desc: desc, date: d, amount: cents})
	slices.SortStableFunc(f.txs, func(a, b alertTx) int { return a.date.Compare(b.date) })
	return id
}

func (f *alertFixture) run(bases map[string]bool) map[string][]alertCandidate {
	out := map[string][]alertCandidate{}
	for _, c := range detectChargeAlerts(f.txs, f.first, bases, f.today) {
		out[c.kind] = append(out[c.kind], c)
	}
	return out
}

func TestNewMerchantAndUnusualAmount(t *testing.T) {
	f := newAlertFixture()
	big := f.add("card", "furniture co", "FURNITURE CO", "2026-09-15", 34000)
	f.add("card", "bakery", "BAKERY", "2026-09-15", 1200)
	spike := f.add("card", "grocer", "GROCER #12 DENVER CO", "2026-09-16", 30000)
	f.add("card", "pharmacy", "PHARMACY", "2026-05-01", 4000)
	f.add("card", "pharmacy", "PHARMACY", "2026-06-01", 4000)
	f.add("card", "pharmacy", "PHARMACY", "2026-07-01", 4000)
	f.add("card", "pharmacy", "PHARMACY", "2026-09-17", 7000) // ordinary variation
	for i, cents := range []int64{2300, 2300, 50000, 2300} {  // a store with the odd big order
		f.add("card", "store", "STORE", fmt.Sprintf("2026-0%d-03", 4+i), cents)
	}
	f.add("card", "store", "STORE", "2026-09-18", 15000)
	got := f.run(nil)
	if n := got["new_merchant"]; len(n) != 1 || n[0].tx != big {
		t.Errorf("new merchant = %+v", n)
	}
	if u := got["unusual_amount"]; len(u) != 1 || u[0].tx != spike {
		t.Errorf("unusual amount = %+v", u)
	}

	// An account with 20 days of history judges nothing as new or unusual.
	f.first["card"] = f.today.AddDate(0, 0, -25)
	if got := f.run(nil); len(got["new_merchant"])+len(got["unusual_amount"]) != 0 {
		t.Errorf("young account = %+v", got)
	}
}

func TestCardTestingAndDuplicate(t *testing.T) {
	f := newAlertFixture()
	small := f.add("card", "zz test", "ZZ TEST", "2026-09-10", 100)
	large := f.add("card", "electronics hub", "ELECTRONICS HUB", "2026-09-12", 48000)
	f.add("chk", "coffee", "COFFEE", "2026-09-14", 575)
	f.add("chk", "coffee", "COFFEE", "2026-09-14", 575) // small repeat purchase
	f.add("chk", "outfitter", "OUTFITTER", "2026-09-14", 8640)
	second := f.add("chk", "outfitter", "OUTFITTER", "2026-09-14", 8640)
	got := f.run(nil)
	if c := got["card_testing"]; len(c) != 1 || c[0].tx != large || c[0].related != small {
		t.Errorf("card testing = %+v", c)
	}
	if d := got["duplicate"]; len(d) != 1 || d[0].tx != second {
		t.Errorf("duplicate = %+v", d)
	}
}

func TestAwayFromHome(t *testing.T) {
	f := newAlertFixture()
	for _, x := range [][3]string{{"hotel", "SUMMIT HOTEL PARK CITY UT", "2026-09-05"}, {"diner", "MOAB DINER MOAB UT", "2026-09-06"},
		{"cafe", "RED CAFE MOAB UT", "2026-09-06"}, {"gas", "FUEL STOP #9 PRICE UT", "2026-09-07"}} {
		f.add("card", x[0], x[1], x[2], 9000)
	}
	lone := f.add("card", "beach shop", "BEACH SHOP MIAMI FL", "2026-09-14", 6000)
	f.add("card", "surf co", "SURF CO TAMPA FL", "2026-09-19", 6000) // lone, but too recent to judge
	f.add("card", "adobe", "ADOBE SAN JOSE CA", "2026-09-12", 5999)
	f.add("card", "corner store", "CORNER STORE", "2026-09-12", 2500) // no location
	got := f.run(map[string]bool{"adobe": true})
	if a := got["away"]; len(a) != 1 || a[0].tx != lone {
		t.Errorf("away = %+v", a)
	}
	if a := f.run(nil)["away"]; len(a) != 2 {
		t.Errorf("without the company base verdict, away = %+v", a)
	}
}

func TestSpendingPace(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse(time.DateOnly, s); return v }
	var txs []alertTx
	add := func(cat, date string, cents int64) {
		txs = append(txs, alertTx{id: fmt.Sprint(len(txs)), category: cat, date: d(date), amount: cents})
		slices.SortStableFunc(txs, func(a, b alertTx) int { return a.date.Compare(b.date) })
	}
	for _, m := range []string{"2026-06", "2026-07", "2026-08"} {
		for _, day := range []string{"03", "10", "17", "24"} {
			add("Dining", m+"-"+day, 15000)
		}
		add("Housing", m+"-02", 200000) // the mortgage posts early every month
		add("Hobbies", m+"-05", 3000)
	}
	add("Housing", "2026-09-02", 200000)
	add("Hobbies", "2026-09-03", 9000)
	add("Dining", "2026-09-03", 25000)
	add("Dining", "2026-09-09", 25000)
	add("", "2026-09-09", 90000)
	got := detectPace(txs, d("2026-09-12"))
	if len(got) != 1 || got[0].key != "2026-09:Dining" || got[0].reasons[0] != "Spending so far is over 50% above the usual by this day of the month" {
		t.Errorf("pace on the 12th = %+v", got)
	}
	if soFar, byNow, usual := paceStats(txs, "Dining", d("2026-09-12")); soFar != 50000 || byNow != 30000 || usual != 60000 {
		t.Errorf("paceStats = %d, %d, %d", soFar, byNow, usual)
	}
	if got := detectPace(txs, d("2026-09-04")); len(got) != 0 {
		t.Errorf("pace on the 4th = %+v", got)
	}
	// More spending later in the month keeps the same key, so a dismissed alert stays dismissed.
	add("Dining", "2026-09-18", 40000)
	if got := detectPace(txs, d("2026-09-20")); len(got) != 1 || got[0].key != "2026-09:Dining" {
		t.Errorf("pace on the 20th = %+v", got)
	}
}

func TestAlertsAfterSync(t *testing.T) {
	posted := time.Now().UTC().Add(-24 * time.Hour).Unix()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"accounts":[{"org":{"name":"Test Bank"},"id":"checking","name":"Checking","balance":"10.00","transactions":[{"id":"sofa","posted":%d,"amount":"-340.00","description":"FURNITURE CO"}]}]}`, posted)
	}))
	defer server.Close()
	a := categoryApp(t)
	a.simplefin = simplefinClient{client: server.Client(), accessURL: strings.Replace(server.URL, "https://", "https://user:pass@", 1), progress: &syncProgress{}}
	if err := a.ensureSimplefinItem(); err != nil {
		t.Fatal(err)
	}
	sync := func() {
		t.Helper()
		if err := a.syncSimplefin(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	count := func(where string) (n int) {
		t.Helper()
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE ` + where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sync()
	if n := count("true"); n != 0 { // a brand-new account judges nothing as new
		t.Fatalf("alerts after the first sync = %d", n)
	}
	old := time.Now().AddDate(0, 0, -100).Format(time.DateOnly)
	if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name) VALUES('old','sf:checking',$1,-500,'CORNER STORE')`, old); err != nil {
		t.Fatal(err)
	}
	sync()
	sync()
	if n := count(`kind='new_merchant' AND transaction_id='sf:checking:sofa'`); n != 1 || count("true") != 1 {
		t.Fatalf("new-merchant alerts after two syncs = %d of %d", n, count("true"))
	}
	if _, err := a.db.Exec(`UPDATE alerts SET dismissed_at=now()`); err != nil {
		t.Fatal(err)
	}
	sync()
	if n := count("dismissed_at IS NULL"); n != 0 {
		t.Errorf("open alerts after dismissing and re-syncing = %d", n)
	}
	if _, err := a.db.Exec(`DROP TABLE merchant_locations`); err != nil { // detection now fails
		t.Fatal(err)
	}
	sync()
}

// jevAlertApp is an app with a card account that has months of history at home (CO), Jev answering from fake.
func jevAlertApp(t *testing.T, answer func(kind, name string) (string, float64)) (*app, *fakeJev, time.Time) {
	t.Helper()
	a := categoryApp(t)
	fake := newFakeJev(t, answer)
	a.jev = fake.client()
	a.jev.usage = a.db
	today, _ := time.Parse(time.DateOnly, "2026-09-20")
	if _, err := a.db.Exec(`UPDATE accounts SET name='Secret Card Name',guessed_type='credit',current=-123456 WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,user_category) VALUES($1,'a',$2,-4000,'GROCER #12 DENVER CO','Groceries')`,
			fmt.Sprint("g", i), today.AddDate(0, 0, -7*i).Format(time.DateOnly)); err != nil {
			t.Fatal(err)
		}
	}
	return a, fake, today
}

func (a *app) addCharge(t *testing.T, id, date, desc string, cents int64) {
	t.Helper()
	if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name) VALUES($1,'a',$2,$3,$4)`, id, date, -cents, desc); err != nil {
		t.Fatal(err)
	}
	if err := assignMerchantKeys(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
}

// requestsAbout returns the decoded request bodies whose state holds kind (merchants or charges).
func (f *fakeJev) requestsAbout(t *testing.T, kind string) []map[string]map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]map[string]any
	for _, b := range f.bodies {
		var req struct {
			State map[string]json.RawMessage `json:"state"`
		}
		json.Unmarshal(b, &req)
		if raw, ok := req.State[kind]; ok {
			var m map[string]map[string]any
			json.Unmarshal(raw, &m)
			out = append(out, m)
		}
	}
	return out
}

func TestJevLocationVerdict(t *testing.T) {
	a, fake, today := jevAlertApp(t, func(kind, name string) (string, float64) {
		if kind == "merchants" && strings.HasPrefix(name, "Adobe") {
			return "base", 0.9
		}
		return "", 0
	})
	a.addCharge(t, "adobe", "2026-09-12", "ADOBE SAN JOSE CA", 5999)
	for range 2 {
		if err := a.detectAlerts(context.Background(), today); err != nil {
			t.Fatal(err)
		}
	}
	asked := fake.requestsAbout(t, "merchants")
	if len(asked) != 1 || len(asked[0]) != 1 {
		t.Fatalf("location requests = %v", asked)
	}
	for _, m := range asked[0] {
		if len(m) != 2 || m["name"] == nil || fmt.Sprint(m["descriptions"]) != "[ADOBE SAN JOSE CA]" {
			t.Errorf("location question sent %v", m)
		}
	}
	var base bool
	var source string
	if err := a.db.QueryRow(`SELECT base,source FROM merchant_locations`).Scan(&base, &source); err != nil || !base || source != "jev" {
		t.Errorf("verdict = %v %q %v", base, source, err)
	}
	var away int
	a.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE kind='away'`).Scan(&away)
	if away != 0 {
		t.Errorf("away alerts for a company base = %d", away)
	}
}

func TestJevTriage(t *testing.T) {
	a, fake, today := jevAlertApp(t, func(kind, name string) (string, float64) {
		if kind == "charges" && name == "FURNITURE CO" {
			return "normal", 0.9
		}
		if kind == "charges" {
			return "suspicious", 0.8
		}
		return "", 0
	})
	a.addCharge(t, "sofa", "2026-09-15", "FURNITURE CO", 34000)
	a.addCharge(t, "tv", "2026-09-16", "ELECTRONICS HUB", 90000)
	if err := a.detectAlerts(context.Background(), today); err != nil {
		t.Fatal(err)
	}
	sent := fake.requestsAbout(t, "charges")
	if len(sent) != 1 || len(sent[0]) != 2 {
		t.Fatalf("triage requests = %v", sent)
	}
	for _, c := range sent[0] {
		if len(c) != 5 || c["account"] != "credit card" || fmt.Sprint(c["usual_categories"]) != "[Groceries]" || c["amount"] == nil || c["reasons"] == nil || c["description"] == nil {
			t.Errorf("triage sent %v", c)
		}
	}
	fake.mu.Lock()
	body := string(fake.bodies[len(fake.bodies)-1])
	fake.mu.Unlock()
	for _, banned := range []string{"Secret Card Name", "1234", "2026-09", `"sofa"`, `"tv"`} {
		if strings.Contains(body, banned) {
			t.Errorf("triage body contains %q: %s", banned, body)
		}
	}
	verdict := func(tx string) (choice string) {
		a.db.QueryRow(`SELECT jev_choice FROM alerts WHERE transaction_id=$1 AND kind='new_merchant'`, tx).Scan(&choice)
		return choice
	}
	if verdict("sofa") != "normal" || verdict("tv") != "suspicious" {
		t.Errorf("verdicts = %q, %q", verdict("sofa"), verdict("tv"))
	}
	var usage int
	a.db.QueryRow(`SELECT COUNT(*) FROM jev_usage`).Scan(&usage)
	if usage != 1 {
		t.Errorf("jev_usage rows = %d", usage)
	}

	// A timeout leaves the next alert open and untriaged, to be asked again.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	a.jev = &jevClient{key: testJevKey, url: slow.URL, minConfidence: 0.5, http: &http.Client{Timeout: 50 * time.Millisecond}}
	a.addCharge(t, "ring", "2026-09-17", "JEWELER", 70000)
	if err := a.detectAlerts(context.Background(), today); err != nil {
		t.Fatal(err)
	}
	if v := verdict("ring"); v != "" {
		t.Errorf("verdict after a timeout = %q", v)
	}
}

func TestAlertsAPI(t *testing.T) {
	a, _, today := jevAlertApp(t, func(string, string) (string, float64) { return "", 0 })
	a.jev = nil
	a.addCharge(t, "sofa", "2026-09-15", "FURNITURE CO", 34000)
	a.addCharge(t, "shop", "2026-09-10", "BEACH SHOP MIAMI FL", 3000)
	for _, m := range []string{"2026-06", "2026-07", "2026-08"} {
		if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,user_category) VALUES($1,'a',$2,-60000,'BISTRO','Dining')`, "d"+m, m+"-10"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,user_category) VALUES('d9','a','2026-09-05',-100000,'BISTRO','Dining')`); err != nil {
		t.Fatal(err)
	}
	if err := a.detectAlerts(context.Background(), today); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE alerts SET jev_choice='normal',jev_confidence=0.9 WHERE kind='new_merchant'`); err != nil {
		t.Fatal(err)
	}
	all, err := a.openAlerts(context.Background(), today)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]alertView{}
	for _, v := range all {
		by[v.Kind] = v
	}
	if v := by["new_merchant"]; !v.Checked || v.Tx == nil || v.Tx.Amount != 34000 || v.Tx.Account != "Secret Card Name" || v.Tx.Date != "2026-09-15" {
		t.Errorf("new merchant = %+v", v)
	}
	if v := by["pace"]; v.Category != "Dining" || v.Month != "2026-09" || v.SoFar != 100000 || v.UsualByNow != 60000 || v.Usual != 60000 {
		t.Errorf("pace = %+v", v)
	}
	away := by["away"]
	if away.Tx == nil || away.Tx.ID != "shop" || away.Checked {
		t.Fatalf("away = %+v, all = %+v", away, all)
	}

	// Both routes through the app; marking the away alert fine remembers the merchant's place as its base.
	var list []alertView
	a.call(t, "GET", "/api/alerts", nil, 200, &list)
	a.call(t, "POST", fmt.Sprintf("/api/alerts/%d/dismiss", away.ID), map[string]any{}, 200, nil)
	a.call(t, "POST", "/api/alerts/999999/dismiss", map[string]any{}, 404, nil)
	var base bool
	var source string
	if err := a.db.QueryRow(`SELECT base,source FROM merchant_locations WHERE merchant_key='beach shop'`).Scan(&base, &source); err != nil || !base || source != "owner" {
		t.Errorf("location after dismissing = %v %q %v", base, source, err)
	}
	if all, _ = a.openAlerts(context.Background(), today); len(all) != len(list)-1 {
		t.Errorf("open after dismissing = %d, before %d", len(all), len(list))
	}
	// Undo reopens it and forgets the base, so a later lone charge there is flagged again.
	a.call(t, "DELETE", fmt.Sprintf("/api/alerts/%d/dismiss", away.ID), nil, 200, nil)
	if all, _ = a.openAlerts(context.Background(), today); len(all) != len(list) {
		t.Errorf("open after undo = %d, want %d", len(all), len(list))
	}
	var n int
	if err := a.db.QueryRow(`SELECT count(*) FROM merchant_locations WHERE merchant_key='beach shop'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("locations after undo = %d %v", n, err)
	}

	req := httptest.NewRequest("POST", fmt.Sprintf("/api/alerts/%d/dismiss", by["pace"].ID), strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.test")
	w := httptest.NewRecorder()
	security(a.routes()).ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("cross-origin dismiss: HTTP %d", w.Code)
	}
}
