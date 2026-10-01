package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMonarchAccountMatching(t *testing.T) {
	ours := []ourAccount{{"chk", "HARBOR CU CHECKING (1111)", ""}, {"k401", "ACME CORP 401(K) PLAN (2222)", ""},
		{"espp", "ESPP", ""}, {"rsu", "RSUs ...555 (555)", ""}, {"card", "Rewards Visa Signature (7777)", "7777"}}
	for name, want := range map[string]string{
		"HARBOR CU CHECKING (...1111)":    "chk",
		"CREDIT CARD (...7777)":           "card", // by mask, though the names differ
		"ACME CORP 401(K) PLAN (...3333)": "k401", // number differs, name matches
		"ESPP (...ESPP)":                  "espp",
		"RSUs ...555 (....555)":           "rsu",
		"NORTHWIND SAVINGS (...4444)":     "",
	} {
		if got, _ := matchMonarchAccount(name, ours); got != want {
			t.Errorf("%s -> %q, want %q", name, got, want)
		}
	}
}

func TestImportMonarch(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assessor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("where"), "R123") || strings.Contains(r.URL.RawQuery, "Example") {
			t.Errorf("assessor query = %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"features":[{"attributes":{"TOTALVALUE":450900,"PROPYEAR":2026}}]}`))
	}))
	defer assessor.Close()
	old := assessorURL
	assessorURL = assessor.URL
	defer func() { assessorURL = old }()
	if _, err := db.Exec(`INSERT INTO items(id) VALUES('simplefin');
		INSERT INTO accounts(id,item_id,name,guessed_type,current) VALUES('sf:chk','simplefin','HARBOR CU CHECKING (1111)','depository',500000),
			('sf:k401','simplefin','ACME CORP 401(K) PLAN (2222)','investment',9000000);
		INSERT INTO transactions(id,account_id,date,amount,name) VALUES
			('sf:1','sf:chk','2026-07-08','-2000','EARLY POSTED'),   -- SimpleFIN history starts 07-08
			('sf:2','sf:chk','2026-07-12','-1000','CAFE');
		INSERT INTO transactions(id,account_id,date,amount,name,pending) VALUES('sf:p','sf:k401','2026-05-02',125000,'contribution',true); -- pending still dedupes
		INSERT INTO balances(account_id,date,current) VALUES('sf:chk','2026-07-10',500000)`); err != nil {
		t.Fatal(err)
	}
	txs := []map[string]string{
		{"Id": "a", "Date": "2026-06-01", "Account": "HARBOR CU CHECKING (...1111)", "Amount": "-5.00", "Merchant": "Safeway", "Original Statement": "SAFEWAY #1", "Category": "Shopping"},
		{"Id": "b", "Date": "2026-07-06", "Account": "HARBOR CU CHECKING (...1111)", "Amount": "-20.00", "Merchant": "Early", "Original Statement": "EARLY", "Category": "Groceries"},  // = sf:1, 2 days apart
		{"Id": "c", "Date": "2026-07-10", "Account": "HARBOR CU CHECKING (...1111)", "Amount": "-10.00", "Merchant": "Cafe", "Original Statement": "CAFE", "Category": "Coffee Shops"}, // = sf:2: copy category
		{"Id": "d", "Date": "2026-07-20", "Account": "HARBOR CU CHECKING (...1111)", "Amount": "-99.00", "Merchant": "Late", "Original Statement": "LATE", "Category": "Shopping"},     // SimpleFIN era, no match: skipped
		{"Id": "e", "Date": "2026-05-01", "Account": "ACME CORP 401(K) PLAN (...3333)", "Amount": "1250.00", "Original Statement": "contribution", "Category": "Uncategorized"},
		{"Id": "f", "Date": "2026-05-01", "Account": "NORTHWIND SAVINGS (...4444)", "Amount": "1.00", "Category": "Interest"},
		{"Id": "g", "Date": "2026-05-02", "Account": "HARBOR CU CHECKING (...1111)", "Amount": "oops", "Category": "Shopping"},
		{"Id": "h", "Date": "2026-05-03", "Account": "HARBOR CU CHECKING (...1111)", "Amount": "12", "Merchant": "Petco", "Original Statement": "PETCO", "Category": "Pets & Stuff"}, // unknown category kept
	}
	bals := []map[string]string{
		{"Date": "2026-07-08", "Balance": "1.00", "Account": "NORTHWIND SAVINGS (...4444)"}, // first day of Monarch's history
		{"Date": "2026-07-09", "Balance": "4000.00", "Account": "HARBOR CU CHECKING (...1111)"},
		{"Date": "2026-07-10", "Balance": "4100.00", "Account": "HARBOR CU CHECKING (...1111)"}, // SimpleFIN has this day: kept
		{"Date": "2026-07-09", "Balance": "450000.00", "Account": "123 Example St"},
	}
	ctx := context.Background()
	prop := "123 Example St=R123"
	if r, err := importMonarch(ctx, db, txs, bals, prop, true); err != nil || r.Imported != 2 {
		t.Fatalf("dry run = %+v, %v", r, err)
	}
	var n int
	if db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE id LIKE 'monarch:%'`).Scan(&n); n != 0 {
		t.Fatalf("dry run saved %d rows", n)
	}
	r, err := importMonarch(ctx, db, txs, bals, prop, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Imported != 2 || r.Covered != 4 || r.Unmatched != 1 || r.Bad != 1 || r.CategoriesCopied != 2 || r.Balances != 2 {
		t.Errorf("report = %+v", r)
	}
	if r.Accounts["NORTHWIND SAVINGS (...4444)"] != "" || r.Accounts["ACME CORP 401(K) PLAN (...3333)"] != "sf:k401" {
		t.Errorf("accounts = %v", r.Accounts)
	}
	check := func(q string, want any, args ...any) {
		t.Helper()
		var got any
		if err := db.QueryRow(q, args...).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %v (%v), want %v", q, got, err, want)
		}
	}
	check(`SELECT user_category FROM transactions WHERE id='monarch:a'`, "Shopping")
	check(`SELECT user_category FROM transactions WHERE id='monarch:h'`, "Pets & Stuff")
	check(`SELECT user_category FROM transactions WHERE id='sf:2'`, "Coffee Shops")
	check(`SELECT user_category FROM transactions WHERE id='sf:1'`, "Groceries") // Monarch's 07-06 copy of SimpleFIN's 07-08 row
	check(`SELECT current FROM balances WHERE account_id='sf:chk' AND date='2026-07-10'`, int64(500000))
	check(`SELECT current FROM balances WHERE account_id='sf:chk' AND date='2026-07-09'`, int64(400000))
	check(`SELECT current FROM accounts WHERE id='property:R123'`, int64(45090000))
	check(`SELECT COUNT(*) FROM balances WHERE account_id='property:R123'`, int64(2)) // Monarch's history + today's assessed value
	check(`SELECT type FROM accounts WHERE id='property:R123'`, "property")

	// Re-run: no duplicates, and a category chosen since the import is kept.
	if _, err := db.Exec(`UPDATE transactions SET user_category='Groceries' WHERE id='monarch:a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := importMonarch(ctx, db, txs, bals, prop, false); err != nil {
		t.Fatal(err)
	}
	check(`SELECT COUNT(*) FROM transactions`, int64(5)) // sf:1, sf:2, sf:p, monarch:a, monarch:h
	check(`SELECT user_category FROM transactions WHERE id='monarch:a'`, "Groceries")
}
