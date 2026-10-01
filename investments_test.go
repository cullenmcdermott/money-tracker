package main

import (
	"context"
	"testing"
)

func TestInvestmentActivityKinds(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO items(id) VALUES('item');
		INSERT INTO accounts(id,item_id,name,guessed_type) VALUES('chk','item','Checking','depository'),('brk','item','Brokerage','investment'),('stock','item','Stock Plan','other')`); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, r := range []struct {
		id, account, date, name string
		amount                  int
		transfer                any
		kind                    string // "" = not in the view
	}{
		{"pay-in", "brk", "2026-08-01", "CASH", 100000, "chk-out", "contribution"},
		{"chk-out", "chk", "2026-08-01", "CONTOSO BUY INVESTMENT", -100000, "pay-in", ""},
		{"k401", "brk", "2026-08-02", "contribution", 125000, nil, "contribution"},
		{"funds", "brk", "2026-08-02", "Funds Received", 50000, nil, "contribution"},
		{"wd", "brk", "2026-08-02", "Withdrawal", -2000, nil, "contribution"},
		{"vest", "stock", "2026-08-03", "RESTRICTED STOCK LAPSE", 0, nil, "contribution"},
		{"espp", "stock", "2026-08-03", "EMPLOYEE STOCK PURCHASE PLAN DEPOSIT", 0, nil, "contribution"},
		{"jto", "stock", "2026-08-04", "Journal To Account XY99", -80000, nil, "contribution"},
		{"jfrm", "brk", "2026-08-09", "JOURNAL FRM ...999", 80000, nil, "contribution"},
		{"div", "brk", "2026-08-05", "Dividend", 3000, nil, "income"},
		{"bankint", "stock", "2026-08-05", "BANK INT 060126-070126 FABRIKAM BANK", 11, nil, "income"},
		{"reinv", "brk", "2026-08-05", "Reinvestment", -3000, nil, "internal"},
		{"sf-div", "brk", "2026-08-06", "ACME 500 INDEX FUND", 1200, nil, "income"}, // SimpleFIN's reinvested dividend pair
		{"sf-reinv", "brk", "2026-08-06", "ACME 500 INDEX FUND", -1200, nil, "internal"},
		{"sf-buy", "brk", "2026-08-07", "ACME 500 INDEX FUND", -50000, nil, "internal"},
		{"buy", "brk", "2026-08-07", "Buy", -50000, nil, "internal"},
		{"sweep", "brk", "2026-08-08", "Sweep in", -1000, nil, "internal"},
		{"sale", "stock", "2026-08-08", "SHARESALE", 80000, nil, "contribution"}, // when a $0 vest gets its value
		{"conv-in", "brk", "2026-08-09", "Conversion (incoming)", -7500, nil, "contribution"},
		{"conv-out", "stock", "2026-08-09", "Conversion (outgoing)", 7500, nil, "contribution"},
	} {
		if _, err := db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,transfer_id) VALUES($1,$2,$3,$4,$5,$6)`, r.id, r.account, r.date, r.amount, r.name, r.transfer); err != nil {
			t.Fatal(err)
		}
		if r.kind != "" {
			want[r.id] = r.kind
		}
	}
	rows, err := db.Query(`SELECT id, kind FROM investment_activity`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		got[id] = kind
	}
	for id, k := range want {
		if got[id] != k {
			t.Errorf("%s = %q, want %q", id, got[id], k)
		}
	}
	var in, out int64
	if err := db.QueryRow(`SELECT (SELECT amount FROM investment_activity WHERE id='conv-in'), (SELECT amount FROM investment_activity WHERE id='conv-out')`).Scan(&in, &out); err != nil || in != 7500 || out != -7500 {
		t.Errorf("conversion amounts = %d, %d (%v); want +7500 into the Roth, -7500 out of the traditional IRA", in, out, err)
	}
	if len(got) != len(want) {
		t.Errorf("view has %d rows, want %d (cash accounts stay out): %v", len(got), len(want), got)
	}
}

func TestInvestmentsBreakdown(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO items(id) VALUES('item');
		INSERT INTO accounts(id,item_id,institution,name,guessed_type) VALUES('brk','item','Contoso Investments','Brokerage','investment'),('stock','item','Fabrikam Securities','Stock Plan','investment');
		INSERT INTO balances(account_id,date,current) VALUES
			('brk','2026-05-31',1000000),('brk','2026-06-30',1200000),('brk','2026-07-31',1300000),
			('stock','2026-05-31',500000),('stock','2026-06-14',500000),('stock','2026-06-15',1300000),('stock','2026-07-31',1250000);
		INSERT INTO transactions(id,account_id,date,amount,name) VALUES
			('dep','brk','2026-06-10',100000,'Funds Received'),      -- added 1,000
			('div','brk','2026-07-01',5000,'Dividend'),              -- dividends 50
			('buy','brk','2026-06-11',-100000,'Buy'),                -- internal
			('vest','stock','2026-06-15',0,'RESTRICTED STOCK LAPSE'),  -- $0: the shares' value arrives with the sale
			('sale','stock','2026-06-16',800000,'SHARESALE')`); err != nil {
		t.Fatal(err)
	}
	got, err := (&app{db: db}).investments(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	accts := got["accounts"].([]investmentAccount)
	by := map[string]investmentAccount{}
	for _, a := range accts {
		by[a.ID] = a
	}
	b, r := by["brk"], by["stock"]
	// "all" starts from the first balance (05-31).
	if b.Start != 1000000 || b.End != 1300000 || b.Added != 100000 || b.Dividends != 5000 || b.Market != 1300000-1000000-100000-5000 {
		t.Errorf("brokerage = %+v", b)
	}
	if r.Added != 800000 || r.End != 1250000 || r.Market != 1250000-500000-800000 {
		t.Errorf("stock = %+v", r)
	}
	if len(b.Months) != 3 || b.Months[1].Month != "2026-06" || b.Months[1].Balance != 1200000 || b.Months[2].Added != 100000 {
		t.Errorf("brokerage months = %+v", b.Months)
	}
	ytd, err := (&app{db: db}).investments(context.Background(), "3m")
	if err != nil {
		t.Fatal(err)
	}
	if got := ytd["from"]; got != "2026-05-01" { // Jul 31 minus 3 months: Go normalizes Apr 31 to May 1
		t.Errorf("3m from = %v", got)
	}
}
