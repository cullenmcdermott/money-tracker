package main

import (
	"math"
	"testing"
	"time"
)

func TestInvest(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db}
	day := func(n int) string { return time.Now().AddDate(0, 0, -n).Format("2006-01-02") }
	if _, err := db.Exec(`INSERT INTO items(id) VALUES('item');
		INSERT INTO accounts(id,item_id,name,guessed_type) VALUES('brk','item','Brokerage','investment'),('ira','item','IRA','investment'),('chk','item','Checking','depository');
		INSERT INTO holdings(account_id,raw) VALUES
			('brk','{"symbol":"AAAAX","description":"Acme 500 Index Fund","shares":"10","market_value":"6000"}'),
			('brk','{"symbol":"BBBBX","description":"Acme Total Intl Stock Idx Fund","shares":"10","market_value":"2000"}'),
			('brk','{"symbol":"CCCCX","description":"Acme Total Bond Market Index Fund","shares":"10","market_value":"2000"}'),
			('brk','{"symbol":"DDDXX","description":"Acme Federal Money Market Fund","shares":"50","market_value":"50"}');
		INSERT INTO fund_classes(symbol,us_stock,intl_stock,bonds,source) VALUES('AAAAX',10000,0,0,'owner'),('BBBBX',0,10000,0,'owner'),('CCCCX',0,0,10000,'owner')`); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		id, date, name string
		amount         int
	}{
		{"b1", day(20), "ACME 500 INDEX ADMIRAL CL", -60000},
		{"b2", day(20), "ACME TOTAL INTL STOCK INDEX ADMIRAL CL", -30000},
		{"b3", day(20), "ACME TOTAL BOND MARKET INDEX ADMIRAL CL", -10000},
		{"div", day(10), "ACME TOTAL INTL STOCK INDEX ADMIRAL CL", 500}, // reinvested dividend pair: not a buy
		{"reinv", day(10), "ACME TOTAL INTL STOCK INDEX ADMIRAL CL", -500},
		{"old", day(400), "ACME TOTAL BOND MARKET INDEX ADMIRAL CL", -1000000}, // before the lookback
		{"mm", day(5), "ACME FEDERAL MONEY MARKET INVESTOR CL", -5000},
		{"odd", day(5), "SOMETHING ELSE ENTIRELY", -7000},
		{"monarch", day(5), "Buy (exchange)", -9000}, // imported from Monarch: names no fund
	} {
		if _, err := db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name) VALUES($1,'brk',$2,$3,$4)`, r.id, r.date, r.amount, r.name); err != nil {
			t.Fatal(err)
		}
	}
	type res struct {
		Funds     []investFund     `json:"funds"`
		Unmatched map[string]int64 `json:"unmatched"`
		Note      string           `json:"note"`
	}
	get := func(amount string) res {
		var r res
		a.call(t, "GET", "/api/invest/brk?amount="+amount, nil, 200, &r)
		return r
	}
	check := func(r res, want map[string][2]float64) { // symbol: target, buy in cents
		t.Helper()
		if len(r.Funds) != 3 {
			t.Fatalf("funds = %+v (money market must be left out)", r.Funds)
		}
		for _, f := range r.Funds {
			w := want[f.Symbol]
			if math.Abs(f.Target-w[0]) > 0.001 || f.Buy != int64(w[1]) {
				t.Errorf("%s target %.3f buy %d, want %.3f %d", f.Symbol, f.Target, f.Buy, w[0], int64(w[1]))
			}
		}
	}

	// Purchases 60/30/10 over the year; $10,000 held at 60/20/20. Bonds are over target, so $4,000 fills the others.
	r := get("400000")
	check(r, map[string][2]float64{"AAAAX": {.6, 200000}, "BBBBX": {.3, 200000}, "CCCCX": {.1, 0}})
	if len(r.Unmatched) != 1 || r.Unmatched["SOMETHING ELSE ENTIRELY"] != 7000 || r.Note != "" {
		t.Errorf("unmatched %v, note %q", r.Unmatched, r.Note)
	}
	// A big enough deposit reaches every target exactly: total $20,000 at 60/30/10.
	check(get("1000000"), map[string][2]float64{"AAAAX": {.6, 600000}, "BBBBX": {.3, 400000}, "CCCCX": {.1, 0}})

	var bad map[string]string
	a.call(t, "PUT", "/api/invest/brk", map[string]any{"mode": "percent", "months": 12, "percent": map[string]int{"AAAAX": 50}}, 400, &bad)
	// A preview tries percentages without saving them.
	var pv res
	a.call(t, "POST", "/api/invest/brk/preview?amount=100000", map[string]any{"mode": "percent", "months": 12, "percent": map[string]int{"AAAAX": 50, "CCCCX": 50}}, 200, &pv)
	check(pv, map[string][2]float64{"AAAAX": {.5, 0}, "BBBBX": {0, 0}, "CCCCX": {.5, 100000}})
	check(get("0"), map[string][2]float64{"AAAAX": {.6, 0}, "BBBBX": {.3, 0}, "CCCCX": {.1, 0}})

	var empty map[string]any
	a.call(t, "GET", "/api/invest/ira", nil, 200, &empty)
	if fs, ok := empty["funds"].([]any); !ok || len(fs) != 0 {
		t.Errorf("an account with no holdings: funds = %v, want []", empty["funds"])
	}
	a.call(t, "PUT", "/api/invest/chk", map[string]any{"mode": "purchases", "months": 12}, 404, &bad)

	a.call(t, "PUT", "/api/invest/brk", map[string]any{"mode": "classes", "months": 12, "classes": []int{50, 30, 20}}, 200, nil)
	check(get("0"), map[string][2]float64{"AAAAX": {.5, 0}, "BBBBX": {.3, 0}, "CCCCX": {.2, 0}})

	// Ignoring the bond fund spreads its share over the others in the mix they're bought in.
	a.call(t, "PUT", "/api/invest/brk", map[string]any{"mode": "purchases", "months": 12, "ignore": []string{"CCCCX"}}, 200, nil)
	check(get("100000"), map[string][2]float64{"AAAAX": {2. / 3, 0}, "BBBBX": {1. / 3, 100000}, "CCCCX": {0, 0}})

	// A short lookback with no buys falls back to what each fund is worth.
	a.call(t, "PUT", "/api/invest/brk", map[string]any{"mode": "purchases", "months": 1, "percent": map[string]int{}}, 200, nil)
	a.db.Exec(`DELETE FROM transactions WHERE id IN ('b1','b2','b3')`)
	if r := get("0"); r.Note == "" || math.Abs(r.Funds[0].Target-.6) > 0.001 {
		t.Errorf("fallback: %+v", r)
	}
}
