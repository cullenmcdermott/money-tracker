package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCleanMerchant(t *testing.T) {
	cases := []struct{ raw, key, display string }{
		{"POS DEBIT 0419 KING SOOPERS #123 DENVER CO", "king soopers", "King Soopers"},
		{"SQ *BLUE BOTTLE COFFEE", "blue bottle coffee", "Blue Bottle Coffee"},
		{"AMZN Mktp US*2K4L", "amazon", "Amazon"},
		{"AMAZON.COM*1A2B3C AMZN.COM/BILL WA", "amazon", "Amazon"},
		{"PAYPAL *STEAMGAMES", "steamgames", "Steamgames"},
		{"TST* JOES DINER", "joes diner", "Joes Diner"},
		{"DD *DOORDASH CHIPOTLE", "doordash chipotle", "Doordash Chipotle"},
		{"UBER *EATS PENDING", "uber eats", "Uber Eats"},
		{"UBER   TRIP HELP.UBER.COM CA", "uber", "Uber"},
		{"WAL-MART SUPERCENTER #1234", "walmart", "Walmart"},
		{"WM SUPERCENTER #5678 DENVER CO", "walmart", "Walmart"},
		{"CHECKCARD 0502 TRADER JOES #55 DENVER CO", "trader joes", "Trader Joes"},
		{"PURCHASE AUTHORIZED ON 04/19 SHELL OIL 57444 HOUSTON TX", "shell oil", "Shell Oil"},
		{"DEBIT CARD PURCHASE 04/19 STARBUCKS STORE 1234", "starbucks store", "Starbucks Store"},
		{"NETFLIX.COM 866-579-7172 CA", "netflix", "Netflix"},
		{"SPOTIFY USA", "spotify usa", "Spotify Usa"},
		{"XCEL ENERGY XXXXX1234", "xcel energy", "Xcel Energy"},
		{"COMCAST CABLE COMM 800-266-2278", "comcast cable comm", "Comcast Cable Comm"},
		{"CVS/PHARMACY #04512", "cvs", "CVS"},
		{"APPLE.COM/BILL 866-712-7753 CA", "apple", "Apple"},
		{"ACH DEBIT XCEL ENERGY-XCEL ENERGY", "xcel energy-xcel energy", "Xcel Energy-xcel Energy"},
		{"7-ELEVEN 32901 DENVER CO", "7-eleven", "7-eleven"},
		{"RECURRING PAYMENT PLANET FITNESS", "planet fitness", "Planet Fitness"},
		{"01/15 CHIPOTLE 1234 AURORA CO", "chipotle", "Chipotle"},
		{"BLUE BOTTLE COFFEE OAKLAND CA", "blue bottle coffee", "Blue Bottle Coffee"},
		{"Joe's Pizza", "joe's pizza", "Joe's Pizza"},
		{"Starbucks", "starbucks", "Starbucks"},
		{"  ", "unknown", "unknown"},
		{"1234", "1234", "1234"},
	}
	for _, c := range cases {
		key, display := cleanMerchant(c.raw)
		if key != c.key || display != c.display {
			t.Errorf("cleanMerchant(%q) = %q, %q; want %q, %q", c.raw, key, display, c.key, c.display)
		}
	}
}

// merchantApp is categoryApp with transactions given merchant keys.
func (a *app) addTx(t *testing.T, id, raw string, amount int) {
	t.Helper()
	a.insertTx(t, id, raw, "", amount, 0, nil)
	if err := assignMerchantKeys(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
}

func (a *app) txField(t *testing.T, col, id string) string {
	t.Helper()
	var v string
	if err := a.db.QueryRow(`SELECT `+col+` FROM transactions WHERE id=$1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (a *app) listedMerchant(t *testing.T, id string) string {
	t.Helper()
	var list []map[string]any
	a.call(t, "GET", "/api/transactions", nil, 200, &list)
	for _, tx := range list {
		if tx["id"] == id {
			return tx["merchant"].(string)
		}
	}
	t.Fatalf("transaction %s not listed", id)
	return ""
}

func TestBackfillOnOpen(t *testing.T) {
	a := categoryApp(t)
	a.insertTx(t, "old", "SQ *BLUE BOTTLE COFFEE", "", -500, 0, nil)
	if err := assignMerchantKeys(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	if got := a.txField(t, "merchant_key", "old"); got != "blue bottle coffee" {
		t.Fatalf("merchant_key = %q", got)
	}
}

func TestRenameAndMergeResolution(t *testing.T) {
	a := categoryApp(t)
	a.addTx(t, "1", "POS DEBIT 0419 KING SOOPERS #123 DENVER CO", -1000)
	a.addTx(t, "2", "KING SOOPERS FUEL #9", -2000)
	a.addTx(t, "3", "SAFEWAY #44", -3000)
	if got := a.listedMerchant(t, "1"); got != "King Soopers" {
		t.Fatalf("auto display = %q", got)
	}
	a.call(t, "PATCH", "/api/merchants/king%20soopers", map[string]string{"display_name": "Soopers"}, 200, nil)
	a.call(t, "PATCH", "/api/merchants/nope", map[string]string{"display_name": "X"}, 404, nil)
	a.call(t, "PATCH", "/api/merchants/safeway", map[string]string{"display_name": " "}, 400, nil)
	if got := a.listedMerchant(t, "1"); got != "Soopers" {
		t.Fatalf("renamed display = %q", got)
	}
	// Merge fuel and safeway into king soopers; a chain (safeway -> fuel -> soopers) stays flat.
	a.call(t, "POST", "/api/merchants/merge", map[string]any{"target": "king soopers", "sources": []string{"king soopers fuel"}}, 200, nil)
	a.call(t, "POST", "/api/merchants/merge", map[string]any{"target": "king soopers fuel", "sources": []string{"safeway"}}, 200, nil)
	a.call(t, "POST", "/api/merchants/merge", map[string]any{"target": "king soopers", "sources": []string{"king soopers"}}, 400, nil)
	a.call(t, "POST", "/api/merchants/merge", map[string]any{"target": "king soopers", "sources": []string{"ghost"}}, 404, nil)
	for _, id := range []string{"1", "2", "3"} {
		if got := a.listedMerchant(t, id); got != "Soopers" {
			t.Errorf("tx %s display = %q, want Soopers", id, got)
		}
	}
	var merchants []map[string]any
	a.call(t, "GET", "/api/merchants", nil, 200, &merchants)
	for _, m := range merchants {
		if m["key"] == "king soopers" && m["transactions"] != float64(3) {
			t.Errorf("target transaction count = %v, want 3", m["transactions"])
		}
		if m["key"] == "safeway" && m["merged_into"] != "king soopers" {
			t.Errorf("safeway merged_into = %v, want flat merge into king soopers", m["merged_into"])
		}
	}
	a.call(t, "PATCH", "/api/merchants/safeway", map[string]string{"display_name": "X"}, 409, nil)
	// Future rows for merged keys follow the target.
	a.addTx(t, "4", "SAFEWAY #77", -500)
	if got := a.listedMerchant(t, "4"); got != "Soopers" {
		t.Fatalf("new row display = %q", got)
	}
}

type suggestion struct {
	Merchant    string  `json:"merchant"`
	Count       int     `json:"uncategorized_count"`
	Total       int64   `json:"uncategorized_total"`
	Category    string  `json:"category"`
	Source      string  `json:"source"`
	Confidence  float64 `json:"confidence"`
	Reason      string  `json:"reason"`
	DisplayName string  `json:"display_name"`
}

func (a *app) suggestions(t *testing.T) map[string]suggestion {
	t.Helper()
	var list []suggestion
	a.call(t, "GET", "/api/suggestions", nil, 200, &list)
	m := make(map[string]suggestion)
	for _, s := range list {
		m[s.Merchant] = s
	}
	return m
}

func TestSuggestionRanking(t *testing.T) {
	a := categoryApp(t)
	// Keyword only.
	a.addTx(t, "k1", "KING SOOPERS #1", -1000)
	a.addTx(t, "k2", "KING SOOPERS #2", -2000)
	// History disagrees with the keyword: 3 categorized as Dining, 1 uncategorized. History must win.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("h%d", i)
		a.addTx(t, id, "SHELL OIL 123", -100)
		a.db.Exec(`UPDATE transactions SET user_category='Dining' WHERE id=$1`, id)
	}
	a.addTx(t, "h9", "SHELL OIL 456", -700)
	// Inconsistent history (2 vs 2) has no majority, so it falls through to the keyword source.
	for i, c := range []string{"Health", "Health", "Other", "Other"} {
		id := fmt.Sprintf("m%d", i)
		a.addTx(t, id, "CVS PHARMACY #1", -100)
		a.db.Exec(`UPDATE transactions SET user_category=$1 WHERE id=$2`, c, id)
	}
	a.addTx(t, "m9", "CVS PHARMACY #2", -100)
	// Nothing to go on: no suggestion.
	a.addTx(t, "z1", "ZZZ UNKNOWN LLC", -100)
	// One consistent-but-thin history entry vs a longer consistent one.
	a.addTx(t, "one", "QQQ CAFE", -100)
	a.db.Exec(`UPDATE transactions SET user_category='Dining' WHERE id='one'`)
	a.addTx(t, "one2", "QQQ CAFE 2", -100)
	a.addTx(t, "big", "WWW MART", -100)
	a.addTx(t, "big2", "WWW MART", -100)
	a.addTx(t, "big3", "WWW MART", -100)
	a.addTx(t, "big4", "WWW MART", -100)
	a.addTx(t, "big5", "WWW MART", -100)
	a.db.Exec(`UPDATE transactions SET user_category='Shopping' WHERE id IN ('big','big2','big3','big4')`)

	got := a.suggestions(t)
	if s := got["king soopers"]; s.Category != "Groceries" || s.Source != "keyword" || s.Count != 2 || s.Total != -3000 {
		t.Errorf("king soopers = %+v", s)
	}
	if s := got["shell oil"]; s.Category != "Dining" || s.Source != "history" || s.Reason != "You've put 3 of 3 Shell Oil transactions in Dining" || s.Count != 1 {
		t.Errorf("shell oil = %+v (history must beat keyword)", s)
	}
	if s := got["cvs"]; s.Source != "keyword" || s.Category != "Health" {
		t.Errorf("cvs = %+v", s)
	}
	if _, ok := got["zzz unknown llc"]; ok {
		t.Error("merchant with no signal got a suggestion")
	}
	thin, thick := got["qqq cafe"], got["www mart"]
	if thin.Confidence >= thick.Confidence || thin.Confidence != 0.5 || thick.Confidence != 0.8 {
		t.Errorf("confidence should grow with count: 1 of 1 = %v, 4 of 4 = %v", thin.Confidence, thick.Confidence)
	}
	// Consistency lowers confidence: 3 of 4 (0.75*0.8 = 0.6).
	a.addTx(t, "mix1", "MIX SHOP", -1)
	a.addTx(t, "mix2", "MIX SHOP", -1)
	a.addTx(t, "mix3", "MIX SHOP", -1)
	a.addTx(t, "mix4", "MIX SHOP", -1)
	a.addTx(t, "mix5", "MIX SHOP", -1)
	a.db.Exec(`UPDATE transactions SET user_category='Shopping' WHERE id IN ('mix1','mix2','mix3')`)
	a.db.Exec(`UPDATE transactions SET user_category='Other' WHERE id='mix4'`)
	if s := a.suggestions(t)["mix shop"]; s.Category != "Shopping" || s.Confidence != 0.6 {
		t.Errorf("mix shop = %+v, want Shopping at 0.60", s)
	}
}

func TestAcceptWithoutRemember(t *testing.T) {
	a := categoryApp(t)
	a.addTx(t, "1", "KING SOOPERS #1", -1000)
	a.addTx(t, "2", "KING SOOPERS #2", -2000)
	var out map[string]any
	a.call(t, "POST", "/api/suggestions/accept", map[string]any{"merchant": "king soopers", "category": "Groceries"}, 200, &out)
	if out["applied"] != float64(2) {
		t.Fatalf("applied = %v", out["applied"])
	}
	if a.effective(t, "1") != "Groceries" || a.txField(t, "user_category", "2") != "Groceries" {
		t.Fatal("existing uncategorized rows should get the category")
	}
	// Not remembered: a later row stays uncategorized.
	a.addTx(t, "3", "KING SOOPERS #3", -500)
	if err := applyRules(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	if got := a.effective(t, "3"); got != "" {
		t.Fatalf("new row = %q, want uncategorized without remember", got)
	}
	a.call(t, "POST", "/api/suggestions/accept", map[string]any{"merchant": "ghost", "category": "Groceries"}, 404, nil)
	a.call(t, "POST", "/api/suggestions/accept", map[string]any{"merchant": "king soopers", "category": ""}, 400, nil)
}

func TestAcceptRememberAppliesToSyncedRowsAndMerges(t *testing.T) {
	now := time.Now().UTC()
	posted := now.Add(-24 * time.Hour).Unix()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"accounts":[{"org":{"name":"Test Bank"},"id":"checking","name":"Checking","balance":"10.00","transactions":[{"id":"n1","posted":%d,"amount":"-4.00","description":"POS DEBIT 0501 KING SOOPERS #9 DENVER CO"},{"id":"n2","posted":%d,"amount":"-6.00","description":"SAFEWAY #3"}]}]}`, posted, posted)
	}))
	defer server.Close()

	a := categoryApp(t)
	a.simplefin = simplefinClient{client: server.Client()}
	a.addTx(t, "1", "KING SOOPERS #1", -1000)
	a.addTx(t, "man", "KING SOOPERS #2", -2000)
	a.call(t, "PATCH", "/api/transactions/man", map[string]string{"category": "Dining"}, 200, nil)
	a.call(t, "POST", "/api/merchants/merge", map[string]any{"target": "king soopers", "sources": []string{"safeway"}}, 404, nil) // safeway unseen yet
	a.addTx(t, "s0", "SAFEWAY #1", -300)
	a.call(t, "POST", "/api/merchants/merge", map[string]any{"target": "king soopers", "sources": []string{"safeway"}}, 200, nil)
	a.call(t, "POST", "/api/suggestions/accept", map[string]any{"merchant": "safeway", "category": "Groceries", "remember": true}, 200, nil) // resolves to target

	if a.effective(t, "1") != "Groceries" || a.effective(t, "s0") != "Groceries" {
		t.Fatal("remembered category should cover existing rows of the merchant and merged keys")
	}
	if got := a.effective(t, "man"); got != "Dining" {
		t.Fatalf("manual category overridden: %q", got)
	}
	// Precedence: a rule beats the remembered merchant category.
	a.addTx(t, "ruled", "KING SOOPERS PHARMACY", -100)
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": "pharmacy", "category": "Health"}, 201, nil)
	if got := a.effective(t, "ruled"); got != "Health" {
		t.Fatalf("rule should win over merchant category, got %q", got)
	}

	accessURL := strings.Replace(server.URL, "https://", "https://user:pass@", 1)
	if _, err := a.simplefin.syncItem(context.Background(), a.db, "item", accessURL); err != nil {
		t.Fatal(err)
	}
	if got := a.effective(t, "sf:checking:n1"); got != "Groceries" {
		t.Errorf("synced King Soopers row = %q, want Groceries", got)
	}
	if got := a.effective(t, "sf:checking:n2"); got != "Groceries" {
		t.Errorf("synced merged-key Safeway row = %q, want Groceries", got)
	}
	// Clearing a manual category hands the row back to the merchant category.
	a.call(t, "PATCH", "/api/transactions/man", map[string]string{"category": ""}, 200, nil)
	if got := a.effective(t, "man"); got != "Groceries" {
		t.Errorf("cleared manual row = %q, want Groceries", got)
	}
}

func TestAcceptLeavesTransfersAndCashflowRawFields(t *testing.T) {
	a := categoryApp(t)
	a.addTx(t, "spend", "ACME PAYMENT", -1000)
	a.addTx(t, "xfer", "ACME PAYMENT", -5000)
	a.addTx(t, "pending", "ACME PAYMENT", -700)
	a.db.Exec(`UPDATE transactions SET transfer_id='other' WHERE id='xfer'`)
	a.db.Exec(`UPDATE transactions SET pending=true WHERE id='pending'`)
	before := func() string {
		var s string
		a.db.QueryRow(`SELECT string_agg(id||COALESCE(transfer_id,'-')||pending::text, ',') FROM (SELECT * FROM transactions ORDER BY id) t`).Scan(&s)
		return s
	}
	raw := before()
	var monthly1, monthly2 []map[string]any
	a.call(t, "GET", "/api/summary/monthly", nil, 200, &monthly1)
	a.call(t, "POST", "/api/suggestions/accept", map[string]any{"merchant": "acme payment", "category": "Other", "remember": false}, 200, nil)
	a.call(t, "POST", "/api/suggestions/accept", map[string]any{"merchant": "acme payment", "category": "Other", "remember": true}, 200, nil)
	if before() != raw {
		t.Fatal("accept changed transfer_id or pending")
	}
	a.call(t, "GET", "/api/summary/monthly", nil, 200, &monthly2)
	if fmt.Sprint(monthly1) != fmt.Sprint(monthly2) {
		t.Fatalf("cashflow totals changed: %v -> %v", monthly1, monthly2)
	}
	if got := a.txField(t, "user_category", "xfer"); got != "" {
		t.Fatalf("transfer got user_category %q", got)
	}
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM cashflow WHERE id='xfer' OR id='pending'`).Scan(&n)
	if n != 0 {
		t.Fatal("transfer or pending row leaked into cashflow")
	}
}

func TestAcceptRememberEmptyClears(t *testing.T) {
	a := categoryApp(t)
	a.addTx(t, "1", "KING SOOPERS #1", -1000)
	a.call(t, "POST", "/api/suggestions/accept", map[string]any{"merchant": "king soopers", "category": "Groceries", "remember": true}, 200, nil)
	if a.effective(t, "1") != "Groceries" {
		t.Fatal("remembered category should apply")
	}
	a.call(t, "POST", "/api/suggestions/accept", map[string]any{"merchant": "king soopers", "category": "", "remember": true}, 200, nil)
	if got := a.effective(t, "1"); got != "" {
		t.Fatalf("after clearing = %q, want uncategorized", got)
	}
}
