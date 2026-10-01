package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func categoryApp(t *testing.T) *app {
	t.Helper()
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO items(id) VALUES('item'); INSERT INTO accounts(id,item_id,name,guessed_type) VALUES('a','item','Checking','depository'),('b','item','Savings','depository')`)
	if err != nil {
		t.Fatal(err)
	}
	return &app{db: db}
}

// call runs one API request and decodes the JSON response into out (if non-nil).
func (a *app) call(t *testing.T, method, path string, body any, want int, out any) {
	t.Helper()
	payload, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(payload)))
	if w.Code != want {
		t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
}

func (a *app) effective(t *testing.T, id string) string {
	t.Helper()
	var c string
	if err := a.db.QueryRow(`SELECT effective_category FROM transactions WHERE id=$1`, id).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (a *app) insertTx(t *testing.T, id, name, merchant string, amount int, pending int, transfer any) {
	t.Helper()
	_, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,merchant,pending,transfer_id) VALUES($1,'a','2026-08-10',$2,$3,$4,$5,$6)`, id, amount, name, merchant, pending != 0, transfer)
	if err != nil {
		t.Fatal(err)
	}
}

func TestManualCategorySurvivesSyncAndRules(t *testing.T) {
	now := time.Now().UTC()
	posted := now.Add(-24 * time.Hour).Unix()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"accounts":[{"org":{"name":"Test Bank"},"id":"checking","name":"Checking","balance":"10.00","transactions":[{"id":"cafe","posted":%d,"amount":"-2.50","description":"POS 123","payee":"Cafe Nero"},{"id":"nflx","posted":%d,"amount":"-9.99","description":"NETFLIX.COM 866"}]}]}`, posted, posted)
	}))
	defer server.Close()
	a := categoryApp(t)
	a.simplefin = simplefinClient{client: server.Client()}
	accessURL := strings.Replace(server.URL, "https://", "https://user:pass@", 1)
	sync := func() {
		t.Helper()
		if _, err := a.simplefin.syncItem(context.Background(), a.db, "item", accessURL); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	cafe, nflx := "sf:checking:cafe", "sf:checking:nflx"
	if a.effective(t, cafe) != "" {
		t.Fatalf("fresh SimpleFIN transaction has category %q", a.effective(t, cafe))
	}

	a.call(t, "PATCH", "/api/transactions/"+cafe, map[string]string{"category": "Dining"}, 200, nil)
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": "netflix", "category": "Subscriptions"}, 201, nil)
	// A rule matching the manual row must not override it, and the new rule backfills the other row.
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": "CAFE", "category": "Other"}, 201, nil)
	if got := a.effective(t, nflx); got != "Subscriptions" {
		t.Errorf("backfill: netflix = %q", got)
	}
	if got := a.effective(t, cafe); got != "Dining" {
		t.Errorf("manual lost to rule: cafe = %q", got)
	}

	sync() // upsert must leave manual and rule results alone, and rules apply to synced rows
	if a.effective(t, cafe) != "Dining" || a.effective(t, nflx) != "Subscriptions" {
		t.Errorf("after re-sync: cafe=%q nflx=%q", a.effective(t, cafe), a.effective(t, nflx))
	}
	// Sync applies rules to newly arrived rows.
	if _, err := a.db.Exec(`DELETE FROM transactions WHERE id=$1`, nflx); err != nil {
		t.Fatal(err)
	}
	sync()
	if got := a.effective(t, nflx); got != "Subscriptions" {
		t.Errorf("sync did not apply rule: %q", got)
	}

	// Clearing the manual category hands the row back to the rules.
	a.call(t, "PATCH", "/api/transactions/"+cafe, map[string]string{"category": ""}, 200, nil)
	if got := a.effective(t, cafe); got != "Other" {
		t.Errorf("after clearing manual: cafe = %q", got)
	}
	// Deleting a rule drops its results.
	var rules []struct {
		ID      int
		Pattern string
	}
	a.call(t, "GET", "/api/rules", nil, 200, &rules)
	if len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
	a.call(t, "DELETE", fmt.Sprintf("/api/rules/%d", rules[0].ID), nil, 200, nil)
	if got := a.effective(t, nflx); got != "" {
		t.Errorf("after deleting rule: netflix = %q", got)
	}
}

func TestRuleConflict(t *testing.T) {
	a := categoryApp(t)
	a.insertTx(t, "provcat", "STARBUCKS", "Starbucks", -500, 0, nil)
	a.insertTx(t, "raw", "Gym", "", -500, 0, nil)
	a.insertTx(t, "both", "Amazon Prime Video", "", -500, 0, nil)
	a.call(t, "PATCH", "/api/transactions/raw", map[string]string{"category": "PERSONAL_CARE"}, 200, nil)
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": "amazon", "category": "Shopping"}, 201, nil)
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": "amazon prime", "category": "Subscriptions"}, 201, nil)
	if got := a.effective(t, "both"); got != "Subscriptions" {
		t.Errorf("longest pattern should win, got %q", got)
	}
	// A rule on the merchant applies to the uncategorized row.
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": "starbucks", "category": "Other"}, 201, nil)
	if got := a.effective(t, "provcat"); got != "Other" {
		t.Errorf("merchant rule: %q", got)
	}
	// Validation.
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": "AMAZON", "category": "X"}, 409, nil)
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": " ", "category": "X"}, 400, nil)
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": "x", "category": ""}, 400, nil)
	a.call(t, "PATCH", "/api/transactions/provcat", map[string]any{}, 400, nil)
	a.call(t, "PATCH", "/api/transactions/nope", map[string]string{"category": "Dining"}, 404, nil)
	a.call(t, "DELETE", "/api/rules/999", nil, 404, nil)

	var cats []string
	a.call(t, "GET", "/api/categories", nil, 200, &cats)
	if len(cats) < len(defaultCategories) || cats[0] != "Income" {
		t.Fatalf("categories = %v", cats)
	}
	if !strings.Contains(strings.Join(cats, ","), "PERSONAL_CARE") {
		t.Errorf("in-use category missing: %v", cats)
	}
}

func TestUncategorizedInboxAndTransferExclusion(t *testing.T) {
	a := categoryApp(t)
	a.insertTx(t, "inbox", "Mystery", "", -1000, 0, nil)
	a.insertTx(t, "pending", "Mystery pending", "", -1000, 1, nil)
	a.insertTx(t, "paired", "Paired transfer", "", -1000, 0, "inbox") // transfer_id set
	a.insertTx(t, "done", "Done", "", -1000, 0, nil)
	a.call(t, "PATCH", "/api/transactions/done", map[string]string{"category": "Dining"}, 200, nil)

	var inbox []map[string]any
	a.call(t, "GET", "/api/transactions?uncategorized=1", nil, 200, &inbox)
	if len(inbox) != 1 || inbox[0]["id"] != "inbox" {
		t.Fatalf("inbox = %v", inbox)
	}

	// A user category (even one named like a transfer code) is a normal category: it must not
	// pull transfers into cashflow, and must not push a real expense out of it.
	a.call(t, "PATCH", "/api/transactions/paired", map[string]string{"category": "Dining"}, 200, nil)
	a.call(t, "PATCH", "/api/transactions/inbox", map[string]string{"category": "TRANSFER_OUT"}, 200, nil)
	a.call(t, "POST", "/api/rules", map[string]string{"pattern": "transfer", "category": "Dining"}, 201, nil)
	var ids []string
	rows, err := a.db.Query(`SELECT id FROM cashflow ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if got := strings.Join(ids, ","); got != "done,inbox" {
		t.Fatalf("cashflow = %s, want done,inbox", got)
	}
	// Summary uses the effective category.
	var summary []map[string]any
	a.call(t, "GET", "/api/summary/categories?month=2026-08", nil, 200, &summary)
	if len(summary) != 2 {
		t.Fatalf("summary = %v", summary)
	}
	got := map[any]any{}
	for _, s := range summary {
		got[s["category"]] = s["amount"]
	}
	if got["Dining"] != float64(1000) || got["TRANSFER_OUT"] != float64(1000) {
		t.Errorf("summary = %v", summary)
	}
	// The listing reports the effective category and whether it is manual.
	var all []map[string]any
	a.call(t, "GET", "/api/transactions", nil, 200, &all)
	for _, tx := range all {
		if tx["id"] == "done" && (tx["category"] != "Dining" || tx["manual"] != true) {
			t.Errorf("done = %v", tx)
		}
	}
}
