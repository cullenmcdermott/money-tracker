package main

import (
	"testing"
	"time"
)

func TestParseHolding(t *testing.T) {
	cost := func(c int64) *int64 { return &c }
	for _, tc := range []struct {
		name, raw string
		ok        bool
		want      position
	}{
		{"index fund with cost", `{"symbol":"ABCDX","description":"ACME 500 INDEX FUND","shares":"100","market_value":"25000.00","cost_basis":"150"}`, true,
			position{Symbol: "ABCDX", Description: "ACME 500 INDEX FUND", Shares: 100, Value: 2500000, Cost: cost(1500000)}},
		{"fractional shares and long per-share cost", `{"symbol":"XYZ","shares":"3.5","market_value":"700.004","cost_basis":"123.456789"}`, true,
			position{Symbol: "XYZ", Shares: 3.5, Value: 70000, Cost: cost(43210)}},
		{"money market with zero cost", `{"symbol":"EXMXX","description":"Example Money Market","shares":"5000","market_value":"5000","cost_basis":"0.00"}`, true,
			position{Symbol: "EXMXX", Description: "Example Money Market", Shares: 5000, Value: 500000}},
		{"brokerage with no cost", `{"symbol":"NWTN","shares":"10","market_value":"1234.5"}`, true,
			position{Symbol: "NWTN", Shares: 10, Value: 123450}},
		{"unvested award", `{"symbol":"","description":"RESTRICTED STOCK UNITS","shares":"0.00","market_value":"4000.00","cost_basis":"0"}`, true,
			position{Description: "RESTRICTED STOCK UNITS", Value: 400000, Unvested: true}},
		{"unvested without spaces", `{"description":"RestrictedStockAward","shares":"0","market_value":"4000"}`, true,
			position{Description: "RestrictedStockAward", Value: 400000, Unvested: true}},
		{"unvested rsu", `{"description":"Contoso RSU grant","shares":"0","market_value":"4000"}`, true,
			position{Description: "Contoso RSU grant", Value: 400000, Unvested: true}},
		{"zero shares, not restricted stock", `{"symbol":"ODD","description":"Something else","shares":"0","market_value":"900"}`, false, position{}},
		{"empty row", `{"symbol":"EXMXX","shares":"0.00","market_value":"0.00"}`, false, position{}},
		{"unparseable value", `{"symbol":"BAD","shares":"1","market_value":"n/a"}`, false, position{}},
		{"numbers instead of strings", `{"symbol":"NUM","shares":2,"market_value":50.25,"cost_basis":20}`, true,
			position{Symbol: "NUM", Shares: 2, Value: 5025, Cost: cost(4000)}},
	} {
		got, ok := parseHolding([]byte(tc.raw))
		if ok != tc.ok {
			t.Errorf("%s: ok = %v", tc.name, ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Symbol != tc.want.Symbol || got.Description != tc.want.Description || got.Shares != tc.want.Shares || got.Value != tc.want.Value || got.Unvested != tc.want.Unvested ||
			(got.Cost == nil) != (tc.want.Cost == nil) || got.Cost != nil && *got.Cost != *tc.want.Cost {
			t.Errorf("%s: got %+v (cost %v), want %+v (cost %v)", tc.name, got, deref(got.Cost), tc.want, deref(tc.want.Cost))
		}
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// holdingsApp has a brokerage with a fund (known cost), a money market (zero cost) and three unvested awards,
// and an account whose brokerage sends no holdings.
func holdingsApp(t *testing.T) *app {
	t.Helper()
	a := categoryApp(t)
	if _, err := a.db.Exec(`INSERT INTO accounts(id,item_id,institution,name,guessed_type) VALUES('brk','item','Contoso Investments','Brokerage','investment'),('ira','item','Fabrikam','IRA','investment');
		INSERT INTO balances(account_id,date,current) VALUES('brk','2026-09-01',3000000),('ira','2026-09-01',800000),('a','2026-09-01',1000);
		INSERT INTO holdings(account_id,raw,synced_at) VALUES
			('brk','{"symbol":"ABCDX","description":"ACME 500 INDEX FUND","shares":"100","market_value":"25000.00","cost_basis":"150"}','2026-09-01T12:00:00Z'),
			('brk','{"symbol":"EXMXX","description":"Example Money Market","shares":"5000","market_value":"5000.00","cost_basis":"0"}','2026-09-01T12:00:00Z'),
			('brk','{"symbol":"","description":"RESTRICTED STOCK UNITS","shares":"0","market_value":"4000.00"}','2026-09-01T12:00:00Z'),
			('brk','{"symbol":"","description":"RESTRICTED STOCK UNITS","shares":"0","market_value":"4000.00"}','2026-09-01T12:00:00Z'),
			('brk','{"symbol":"","description":"RESTRICTED STOCK UNITS","shares":"0","market_value":"4000.00"}','2026-09-01T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestInvestmentPositions(t *testing.T) {
	a := holdingsApp(t)
	var nwBefore []map[string]any
	a.call(t, "GET", "/api/networth", nil, 200, &nwBefore)
	var got struct {
		Accounts []investmentAccount `json:"accounts"`
	}
	a.call(t, "GET", "/api/investments?period=all", nil, 200, &got)
	by := map[string]investmentAccount{}
	for _, ia := range got.Accounts {
		by[ia.ID] = ia
	}
	b := by["brk"]
	if len(b.Positions) != 2 || b.Positions[0].Symbol != "ABCDX" || b.Positions[1].Symbol != "EXMXX" {
		t.Fatalf("positions = %+v", b.Positions)
	}
	if b.Unvested != 1200000 || b.Cost != 1500000 || b.Gain != 1000000 || b.NoCost != 500000 || b.HoldingsAt == "" {
		t.Errorf("brokerage totals = unvested %d, cost %d, gain %d, no cost %d, at %q", b.Unvested, b.Cost, b.Gain, b.NoCost, b.HoldingsAt)
	}
	if ira := by["ira"]; ira.Positions != nil || ira.HoldingsAt != "" {
		t.Errorf("account without holdings = %+v", ira)
	}
	var nwAfter []map[string]any
	a.call(t, "GET", "/api/networth", nil, 200, &nwAfter)
	if len(nwAfter) != 1 || nwAfter[0]["total"] != float64(3801000) || len(nwBefore) != 1 || nwBefore[0]["total"] != nwAfter[0]["total"] {
		t.Errorf("net worth = %v, want the balances only (3,801,000)", nwAfter)
	}
}

func TestAllocation(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	classes := map[string]fundClass{
		"USIDX": {Split: [5]int{10000, 0, 0, 0, 0}, Source: "sec", AsOf: "2026-06-30", saved: now.AddDate(0, 0, -10)},
		"BALX":  {Split: [5]int{0, 5000, 5000, 0, 0}, Source: "sec", AsOf: "2026-06-30", saved: now.AddDate(0, 0, -10)},
		"TRUST": {Split: [5]int{9000, 0, 1000, 0, 0}, Source: "owner"},
		"OLDX":  {Split: [5]int{10000, 0, 0, 0, 0}, Source: "sec", AsOf: "2026-01-31", saved: now.AddDate(0, 0, -121)},
		"ACME":  {Split: [5]int{10000, 0, 0, 0, 0}, Source: "stock", saved: now.AddDate(-1, 0, 0)},
	}
	var mixed allocation
	for _, p := range []position{{Symbol: "USIDX", Value: 6000000}, {Symbol: "BALX", Value: 4000000}} {
		mixed.add(p.Value, classify(p, classes, now))
	}
	if mixed != (allocation{USStock: 6000000, IntlStock: 2000000, Bonds: 2000000}) {
		t.Errorf("mixed portfolio = %+v, want 60%% US, 20%% intl, 20%% bonds", mixed)
	}
	for _, tc := range []struct {
		p      position
		want   [5]int
		source string // "" = unclassified
	}{
		{position{Symbol: "TRUST"}, [5]int{9000, 0, 1000, 0, 0}, "owner"},
		{position{Symbol: "EXMXX"}, [5]int{0, 0, 0, 10000, 0}, "money_market"},
		{position{Symbol: "CASHQ", Description: "Example Treasury Money Market Fund"}, [5]int{0, 0, 0, 10000, 0}, "money_market"},
		{position{Symbol: "OLDX"}, [5]int{}, ""},
		{position{Symbol: "ACME"}, [5]int{10000, 0, 0, 0, 0}, "stock"},
		{position{Symbol: "NOPE"}, [5]int{}, ""},
	} {
		c := classify(tc.p, classes, now)
		if c == nil && tc.source != "" || c != nil && (c.Source != tc.source || c.Split != tc.want) {
			t.Errorf("%s = %+v, want %v from %q", tc.p.Symbol, c, tc.want, tc.source)
		}
	}
	// The owner's split replaces an SEC one for the same symbol.
	classes["USIDX"] = fundClass{Split: [5]int{0, 0, 10000, 0, 0}, Source: "owner"}
	if c := classify(position{Symbol: "USIDX"}, classes, now); c == nil || c.Source != "owner" {
		t.Errorf("owner override = %+v", c)
	}
	var un allocation
	un.add(250000, classify(position{Symbol: "NOPE"}, classes, now))
	if un != (allocation{Unclassified: 250000}) {
		t.Errorf("unclassified = %+v", un)
	}
}

func TestFundClassRoutes(t *testing.T) {
	a := holdingsApp(t)
	a.call(t, "PUT", "/api/fund-classes/ABCDX", map[string]any{"split": []int{60, 0, 35, 0, 0}}, 400, nil)
	a.call(t, "PUT", "/api/fund-classes/ABCDX", map[string]any{"split": []int{60, 0, 40, 0, -0}}, 200, nil)
	var got struct {
		Allocation allocation          `json:"allocation"`
		Accounts   []investmentAccount `json:"accounts"`
	}
	a.call(t, "GET", "/api/investments", nil, 200, &got)
	// $25,000 fund at 60/40 and a $5,000 money market; the unvested awards stay out.
	if got.Allocation != (allocation{USStock: 1500000, Bonds: 1000000, Cash: 500000}) {
		t.Errorf("portfolio = %+v", got.Allocation)
	}
	for _, ia := range got.Accounts {
		if ia.ID == "brk" && (ia.Allocation != got.Allocation || ia.Positions[0].Class == nil || ia.Positions[0].Class.Source != "owner") {
			t.Errorf("brokerage = %+v, first position class %+v", ia.Allocation, ia.Positions[0].Class)
		}
	}
	a.call(t, "DELETE", "/api/fund-classes/ABCDX", nil, 200, nil)
	a.call(t, "GET", "/api/investments", nil, 200, &got)
	if got.Allocation.Unclassified != 2500000 {
		t.Errorf("after clearing = %+v", got.Allocation)
	}
}

// A sync that reaches only some accounts writes today's balance for just those; the rest still count at their
// latest earlier balance instead of dropping out of that day's total.
func TestNetWorthCarriesForward(t *testing.T) {
	a := holdingsApp(t)
	if _, err := a.db.Exec(`INSERT INTO balances(account_id,date,current) VALUES('brk','2026-10-01',3100000)`); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	a.call(t, "GET", "/api/networth", nil, 200, &got)
	if len(got) != 2 || got[0]["date"] != "2026-09-01" || got[0]["total"] != float64(3801000) || got[1]["date"] != "2026-10-01" || got[1]["total"] != float64(3901000) {
		t.Errorf("net worth = %v, want 3,801,000 on 09-01 then 3,901,000 on 10-01", got)
	}
	if _, err := a.db.Exec(`INSERT INTO balances(account_id,date,current) VALUES('a','2026-10-02',-5000)`); err != nil {
		t.Fatal(err)
	}
	a.call(t, "GET", "/api/networth", nil, 200, &got)
	if last := got[2]; last["total"] != float64(3895000) || last["assets"] != float64(3900000) || last["debts"] != float64(5000) {
		t.Errorf("10-02 = %v, want assets 3,900,000 and debts 5,000", last)
	}
}
