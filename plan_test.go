package main

import (
	"context"
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// planApp has five complete months of history before 2026-09-20 ($5,000 in and $3,000 out each month, from
// checking), a 401(k) with $2,300 of payroll contributions, and a brokerage funded only by a transfer from checking.
func planApp(t *testing.T) (*app, time.Time) {
	t.Helper()
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db}
	if _, err := db.Exec(`INSERT INTO items(id) VALUES('item');
		INSERT INTO accounts(id,item_id,name,guessed_type,current) VALUES
			('chk','item','Checking','depository',1400000),('sav','item','High-Yield Savings','depository',3800000),
			('roth','item','Roth IRA','investment',4800000),('k401','item','401(k)','investment',19600000),
			('brk','item','Individual Brokerage','investment',12400000),('hsa','item','HSA','investment',500000),
			('card','item','Rewards Card','credit',-250000);
		INSERT INTO holdings(account_id,raw) VALUES('brk','{"description":"RESTRICTED STOCK UNITS","shares":"0","market_value":"4000"}')`); err != nil {
		t.Fatal(err)
	}
	for i, m := range []string{"2026-04", "2026-05", "2026-06", "2026-07", "2026-08", "2026-09"} {
		if _, err := db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name) VALUES($1,'chk',$2,500000,'PAYROLL'),($3,'chk',$2,-300000,'GROCER')`,
			"in"+m, m+"-05", "out"+m); err != nil {
			t.Fatal(err)
		}
		if i < 5 {
			if _, err := db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name) VALUES($1,'k401',$2,46000,'Contribution')`, "c"+m, m+"-15"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,transfer_id) VALUES
		('to-brk','chk','2026-06-10',-1000000,'TRANSFER TO BROKERAGE','brk-in'),('brk-in','brk','2026-06-10',1000000,'Funds Received','to-brk')`); err != nil {
		t.Fatal(err)
	}
	today, _ := time.Parse(time.DateOnly, "2026-09-20")
	return a, today
}

func TestPlanBaseline(t *testing.T) {
	a, today := planApp(t)
	// An equity awards account: an RSU vest and an ESPP purchase arrive as $0 deposits and get their value when
	// sold; the proceeds are journaled to the brokerage. A Roth conversion, and a stock sale in an account with no
	// vest or purchase to tie it to. Savings pays interest.
	if _, err := a.db.Exec(`INSERT INTO accounts(id,item_id,name,guessed_type,current) VALUES('stock','item','Equity Awards','investment',500000),
			('odd','item','Fabrikam Shares','investment',100000);
		INSERT INTO transactions(id,account_id,date,amount,name) VALUES
			('lapse','stock','2026-03-30',0,'RESTRICTED STOCK LAPSE'),('rsu-sale','stock','2026-04-02',600000,'SHARESALE'),
			('espp','stock','2026-05-15',0,'EMPLOYEE STOCK PURCHASE PLAN DEPOSIT'),('espp-sale','stock','2026-05-20',300000,'SHARESALE'),
			('jto','stock','2026-05-21',-900000,'Journal To Account XY99'),('jfrm','brk','2026-05-22',900000,'JOURNAL FRM ...999'),
			('conv','roth','2026-06-01',700000,'Conversion (incoming)'),('odd-sale','odd','2026-06-03',200000,'SHARESALE'),
			('int','sav','2026-05-31',15000,'INTEREST PAYMENT')`); err != nil {
		t.Fatal(err)
	}
	b, err := a.planBaseline(context.Background(), today)
	if err != nil {
		t.Fatal(err)
	}
	// September is not complete, so the baseline is April to August: 5 months.
	// Interest is left out of income: savings grows at the 1% it paid instead.
	if b.Months != 5 || b.Income != 500000 || b.Spending != 300000 || b.Interest != 3000 || b.Unvested != 400000 {
		t.Errorf("baseline = %d months, income %d, spending %d, interest %d, unvested %d", b.Months, b.Income, b.Spending, b.Interest, b.Unvested)
	}
	if len(b.IncomeSources) != 1 || b.IncomeSources[0] != (planSource{"PAYROLL", 6000000, 5, "2026-08-05", false}) {
		t.Errorf("income sources = %+v", b.IncomeSources)
	}
	if len(b.UnclearSales) != 1 || b.UnclearSales[0].AccountID != "odd" || b.UnclearSales[0].Amount != 200000 {
		t.Errorf("unclear sales = %+v", b.UnclearSales)
	}
	split := map[string][5]int64{ // deposits, ESPP, RSU, unclear, internal; a year
		"k401":  {230000 * 12 / 5, 0, 0, 0, 0},
		"stock": {0, 300000 * 12 / 5, 600000 * 12 / 5, 0, 0}, // the RSU sale's vest was before the window
		"brk":   {0, 0, 0, 0, 900000 * 12 / 5},
		"roth":  {0, 0, 0, 0, 700000 * 12 / 5},
		"odd":   {0, 0, 0, 200000 * 12 / 5, 0},
	}
	// The RSU and ESPP sales are stock pay; the brokerage got the money moved from checking, so the surplus goes there.
	if b.Stock != (600000+300000)*12/5 || b.SurplusAccount != "brk" {
		t.Errorf("stock pay %d, surplus account %q", b.Stock, b.SurplusAccount)
	}
	for _, ac := range b.Accounts {
		if got := [5]int64{ac.Deposits, ac.ESPP, ac.RSU, ac.Unclear, ac.Internal}; got != split[ac.ID] {
			t.Errorf("%s contributions = %v, want %v", ac.ID, got, split[ac.ID])
		}
	}
	want := map[string]struct {
		include bool
		bucket  string
		growth  float64
		contrib int64
	}{
		"chk":   {true, "cash", 0, 0},
		"sav":   {true, "cash", 1, 0},
		"roth":  {true, "roth", 6, 0},
		"k401":  {true, "pretax", 6, 230000 * 12 / 5}, // payroll counts, annualized from 5 months
		"brk":   {true, "taxable", 6, 0},              // a transfer from checking is in the surplus; a journal is internal
		"hsa":   {true, "pretax", 6, 0},
		"card":  {false, "cash", 0, 0},
		"stock": {true, "taxable", 6, 0}, // its sales are stock pay
		"odd":   {true, "taxable", 6, 0},
	}
	for _, ac := range b.Accounts {
		w, ok := want[ac.ID]
		if !ok || ac.Include != w.include || ac.Bucket != w.bucket || ac.Growth != w.growth || ac.Contribution != w.contrib {
			t.Errorf("%s = %+v, want %+v", ac.ID, ac, w)
		}
		if ac.ID == "hsa" && ac.PenaltyFreeAge != 65 || ac.ID == "k401" && ac.PenaltyFreeAge != 59.5 {
			t.Errorf("%s penalty-free age = %v", ac.ID, ac.PenaltyFreeAge)
		}
	}
	if len(b.Accounts) != len(want) {
		t.Errorf("accounts = %d", len(b.Accounts))
	}
}

func TestClassifySales(t *testing.T) {
	const lapse, espp, sale = "RESTRICTED STOCK LAPSE", "EMPLOYEE STOCK PURCHASE PLAN DEPOSIT", "SHARESALE"
	kindOf := func(rows ...contribRow) (s planAccount, unclear int) {
		sums, u := classifyContributions(rows, "2025-01-01", nil)
		return *sums["a"], len(u)
	}
	row := func(date, name string, amount int64) contribRow { return contribRow{"a", date, name, amount, ""} }
	// Both kinds in the account: a sale far from the lapse and the purchase is left out and listed.
	if s, u := kindOf(row("2025-11-15", lapse, 0), row("2025-11-30", espp, 0), row("2025-12-10", sale, 100)); u != 1 || s.ESPP != 0 || s.RSU != 0 || s.Unclear != 100 {
		t.Errorf("sale 10 days after the purchase and 25 after the lapse: %+v, %d unclear", s, u)
	}
	// Two days after a lapse, with no purchase in the week: RSU.
	if s, u := kindOf(row("2025-11-15", lapse, 0), row("2025-11-30", espp, 0), row("2025-11-17", sale, 100)); u != 0 || s.RSU != 100 {
		t.Errorf("sale after a lapse: %+v, %d unclear", s, u)
	}
	// Deposits of both kinds in the week: can't tell.
	if s, u := kindOf(row("2025-11-15", lapse, 0), row("2025-11-16", espp, 0), row("2025-11-17", sale, 100)); u != 1 || s.Unclear != 100 {
		t.Errorf("sale after both: %+v, %d unclear", s, u)
	}
	// A sale ordered before its same-day deposit still finds it.
	if s, u := kindOf(row("2025-10-01", lapse, 0), row("2025-12-10", sale, 100), row("2025-12-10", espp, 0)); u != 0 || s.ESPP != 100 {
		t.Errorf("same-day purchase: %+v, %d unclear", s, u)
	}
	// One kind only: the sale is that kind however long after; no deposits at all: unclear.
	if s, u := kindOf(row("2025-03-01", lapse, 0), row("2025-12-10", sale, 100)); u != 0 || s.RSU != 100 {
		t.Errorf("only RSU: %+v, %d unclear", s, u)
	}
	if s, u := kindOf(row("2025-03-01", espp, 0), row("2025-12-10", sale, 100)); u != 0 || s.ESPP != 100 {
		t.Errorf("only ESPP: %+v, %d unclear", s, u)
	}
	if _, u := kindOf(row("2025-12-10", sale, 100)); u != 1 {
		t.Errorf("no deposits: %d unclear", u)
	}
}

func TestPlanInterestGrowth(t *testing.T) {
	a, today := planApp(t)
	// Savings earned $2,000 over the five months (its merchant is the bank, its name says interest) on an average of $100,000, then was drawn to $38,000: 4.8% a
	// year, not 12.6% of what is left. Checking's "dividend" and a savings account's $1,000 on $10,000 are above
	// any cash rate: they stay income, and the savings account keeps its default 2%.
	if _, err := a.db.Exec(`INSERT INTO accounts(id,item_id,name,guessed_type,current) VALUES('sav2','item','Old Savings','depository',1000000);
		INSERT INTO transactions(id,account_id,date,amount,name) VALUES
			('div','chk','2026-07-31',500000,'DIVIDEND'),('si2','sav2','2026-06-30',100000,'INTEREST PAYMENT');
		INSERT INTO transactions(id,account_id,date,amount,name,merchant) VALUES('si','sav','2026-06-30',200000,'Interest Paid','Contoso Bank')`); err != nil {
		t.Fatal(err)
	}
	for d, bal := range map[string]int{"2026-04-10": 12000000, "2026-06-10": 10000000, "2026-08-10": 8000000, "2026-03-31": 100} { // the last is before the window
		if _, err := a.db.Exec(`INSERT INTO balances(account_id,date,current) VALUES('sav',$1,$2)`, d, bal); err != nil {
			t.Fatal(err)
		}
	}
	b, err := a.planBaseline(context.Background(), today)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		interest int64
		growth   float64
	}{"sav": {200000 * 12 / 5, 5}, "sav2": {0, 2}, "chk": {0, 0}}
	for _, ac := range b.Accounts {
		if w, ok := want[ac.ID]; ok && (ac.Interest != w.interest || ac.Growth != w.growth) {
			t.Errorf("%s interest %d growth %v, want %+v", ac.ID, ac.Interest, ac.Growth, w)
		}
	}
	if b.Interest != 40000 || b.Income != (2500000+500000+100000)/5 {
		t.Errorf("interest %d, income %d", b.Interest, b.Income)
	}
	got := map[string]planSource{}
	for _, s := range b.IncomeSources {
		got[s.Name] = s
	}
	if got["DIVIDEND"].Amount != 500000*12/5 || got["INTEREST PAYMENT"] != (planSource{"INTEREST PAYMENT", 100000 * 12 / 5, 1, "2026-06-30", false}) {
		t.Errorf("income sources = %+v", b.IncomeSources)
	}
}

// planCase is a plan with no inflation and no growth on extra savings, so years can be checked by hand.
func planCase(retire int, accounts ...projAccount) planInputs {
	return planInputs{BirthYear: 1980, StartYear: 2026, RetireAge: retire, EndAge: 90, SSAge: 200, RetireSpendPct: 100, Accounts: accounts}
}

func TestProjectByHand(t *testing.T) {
	// Born 1980, so 46 in 2026; retires at 47; the plan ends at 48: three years.
	in := planCase(47, projAccount{ID: "cash", Bucket: "cash", Balance: 800000, PenaltyFreeAge: 59.5},
		projAccount{ID: "k", Bucket: "pretax", Balance: 10000000, Growth: 5, Contribution: 600000, PenaltyFreeAge: 59.5})
	in.EndAge, in.Income, in.Spending = 48, 500000, 400000
	years, runOut := project(in)
	want := []planYear{
		// Working: $12,000 surplus to extra savings; $6,000 contributed; growth 5% of (100,000 + 6,000/2) = 5,150.
		{Year: 2026, Age: 46, Income: 6000000, Spending: 4800000, Contributions: 600000, Assets: 800000 + 11115000 + 1200000},
		// Retired: $48,000 needed. Extra savings 12,000, cash 8,000, then 28,000 from the 401(k) before 59½:
		// 28,000 / (1 - 20% tax - 10% penalty) = 40,000, of which 12,000 tax. Growth 5% of (111,150 - 40,000/2).
		{Year: 2027, Age: 47, Spending: 4800000, Withdrawn: 6000000, Taxes: 1200000, Assets: 7570750},
		// 48,000 / 0.7 = 68,571.43, tax 20,571.43; growth 5% of (75,707.50 - 34,285.715) = 2,071.09.
		{Year: 2028, Age: 48, Spending: 4800000, Withdrawn: 6857143, Taxes: 2057143, Assets: 920716},
	}
	if len(years) != len(want) || runOut != 0 {
		t.Fatalf("years = %+v, run out %d", years, runOut)
	}
	for i := range want {
		if years[i] != want[i] {
			t.Errorf("year %d = %+v\n         want %+v", i, years[i], want[i])
		}
	}
}

func TestProjectScenarios(t *testing.T) {
	// Surplus goes to extra savings, growing at 2% before inflation.
	in := planCase(65)
	in.EndAge, in.Income, in.Spending, in.ExtraGrowth = 46, 1000000, 750000, 2
	if y, _ := project(in); y[0].Assets != 3000000+30000 {
		t.Errorf("working surplus: assets %d, want 30,300.00", y[0].Assets)
	}
	// After 59½ a pre-tax withdrawal pays 20% and no penalty.
	in = planCase(60, projAccount{ID: "k", Bucket: "pretax", Balance: 10000000, PenaltyFreeAge: 59.5})
	in.BirthYear, in.EndAge, in.Spending = 1966, 60, 400000
	if y, _ := project(in); y[0].Withdrawn != 6000000 || y[0].Taxes != 1200000 {
		t.Errorf("after 59½: %+v", y[0])
	}
	// $100,000 of cash and $20,000 a year of spending lasts five years; the sixth is short.
	in = planCase(46, projAccount{ID: "c", Bucket: "cash", Balance: 10000000})
	in.EndAge, in.Spending = 55, 2000000/12
	years, runOut := project(in)
	if runOut != 51 || years[4].Shortfall != 0 || years[5].Shortfall == 0 || years[5].Assets != 0 {
		t.Errorf("runs out at %d; years %+v", runOut, years[4:6])
	}
	// A mortgage paid off in 2041 lowers spending from then on; a car in 2029 adds to that year only.
	in = planCase(65)
	in.EndAge, in.Spending = 70, 500000
	in.Events = []planEvent{{Kind: "spending", Name: "Mortgage paid off", Year: 2041, Amount: -2400000}, {Kind: "expense", Name: "Car", Year: 2029, Amount: 4000000}}
	years, _ = project(in)
	for _, y := range years {
		want := int64(6000000)
		if y.Year >= 2041 {
			want -= 2400000
		}
		if y.Year == 2029 {
			want += 4000000
		}
		if y.Spending != want {
			t.Errorf("%d spending = %d, want %d", y.Year, y.Spending, want)
		}
	}
	// Stock pay of $5,000 a year from $12,000 granted: two full years, then the $2,000 left.
	in = planCase(65)
	in.EndAge, in.Stock = 52, planStockIn{Yearly: 500000, Until: "granted", Granted: 1200000}
	years, _ = project(in)
	for i, y := range years {
		want := []int64{500000, 500000, 200000, 0, 0, 0, 0}[i]
		if y.Income != want {
			t.Errorf("%d income = %d, want %d", y.Year, y.Income, want)
		}
	}
	// On October 1, three months of the year are left: a quarter of the year's pay, spending, contributions and
	// stock pay; a one-time expense still counts in full. The next year is whole.
	in = planCase(65, projAccount{ID: "k", Bucket: "pretax", Contribution: 1200000, PenaltyFreeAge: 59.5})
	in.EndAge, in.Income, in.Spending, in.Elapsed = 47, 1000000, 400000, 0.75
	in.Events = []planEvent{{Kind: "expense", Name: "Roof", Year: 2026, Amount: 100000}}
	in.Stock = planStockIn{Yearly: 500000, Until: "retire"}
	years, _ = project(in)
	if y := years[0]; y.Income != 3000000+125000 || y.Spending != 1200000+100000 || y.Contributions != 300000 {
		t.Errorf("rest of the first year = %+v", y)
	}
	if y := years[1]; y.Income != 12000000+500000 || y.Spending != 4800000 || y.Contributions != 1200000 {
		t.Errorf("first full year = %+v", y)
	}
	// Retiring now, at 46, stops pay, contributions and stock pay at once.
	in = planCase(46, projAccount{ID: "k", Bucket: "pretax", Balance: 10000000, Contribution: 600000, PenaltyFreeAge: 59.5})
	in.EndAge, in.Income, in.Stock = 47, 1000000, planStockIn{Yearly: 500000, Until: "retire"}
	for _, y := range func() []planYear { y, _ := project(in); return y }() {
		if y.Income != 0 || y.Contributions != 0 {
			t.Errorf("%d retired: income %d, contributions %d", y.Year, y.Income, y.Contributions)
		}
	}
	// Retiring at 48 keeps the first two years of stock pay; "none" leaves it out.
	in.RetireAge = 48
	in.EndAge = 49
	years, _ = project(in)
	for i, y := range years {
		want := int64(0)
		if i < 2 {
			want = 12000000 + 500000
		}
		if y.Income != want {
			t.Errorf("%d income = %d, want %d", y.Year, y.Income, want)
		}
	}
	in.Stock.Until = "none"
	if y, _ := project(in); y[0].Income != 12000000 {
		t.Errorf("no stock pay: income %d", y[0].Income)
	}
	// Retired at 60: ,000 of spending plus $15,000 of insurance until 65, then $6,000 of health costs; the
	// mortgage's $12,000 a year stops after its last payment in 2030.
	in = planCase(60)
	in.BirthYear, in.EndAge, in.Spending, in.HealthBefore65, in.RetireExtra = 1966, 66, 400000, 1500000, 600000
	in.MortgageMonthly, in.MortgageLastYear = 100000, 2030
	years, _ = project(in)
	for i, want := range []int64{6300000, 6300000, 6300000, 6300000, 6300000, 5400000 - 1200000, 5400000 - 1200000} {
		if years[i].Spending != want {
			t.Errorf("%d (age %d) spending = %d, want %d", years[i].Year, years[i].Age, years[i].Spending, want)
		}
	}
}

func TestPlanAPI(t *testing.T) {
	a, _ := planApp(t)
	var got struct {
		Doc        *planDoc     `json:"doc"`
		Baseline   planBaseline `json:"baseline"`
		Projection *struct {
			Years  []planYear `json:"years"`
			RunOut int        `json:"run_out"`
		} `json:"projection"`
	}
	a.call(t, "GET", "/api/plan", nil, 200, &got)
	if got.Doc != nil || got.Projection != nil || len(got.Baseline.Accounts) == 0 {
		t.Fatalf("before setup = %+v", got)
	}
	var bad map[string]string
	a.call(t, "PUT", "/api/plan", map[string]any{"birth_year": 1985, "retire_age": 5}, 400, &bad)
	if !strings.Contains(bad["error"], "retire_age") {
		t.Errorf("validation error = %q", bad["error"])
	}
	a.call(t, "PUT", "/api/plan", map[string]any{"birth_year": 1985, "events": []map[string]any{{"kind": "expense", "name": "Car", "year": 2029, "amount": -5}}}, 400, &bad)
	if !strings.Contains(bad["error"], "events[0].amount") {
		t.Errorf("event validation error = %q", bad["error"])
	}
	a.call(t, "PUT", "/api/plan", map[string]any{"birth_year": 1985}, 200, &got)
	if got.Doc == nil || got.Projection == nil || got.Projection.Years[0].Age != time.Now().Year()-1985 {
		t.Fatalf("after setup = %+v", got.Projection)
	}

	// A what-if returns both projections and saves nothing.
	var what struct {
		Projection     planProjection `json:"projection"`
		DataProjection planProjection `json:"data_projection"`
	}
	a.call(t, "POST", "/api/plan/projection", map[string]any{"birth_year": 1985, "spending_monthly": 100}, 200, &what)
	if what.Projection.Years[1].Spending != 1200 || what.DataProjection.Years[1].Spending != 300000*12 {
		t.Errorf("what-if spending = %d, data %d", what.Projection.Years[1].Spending, what.DataProjection.Years[1].Spending)
	}
	a.call(t, "GET", "/api/plan", nil, 200, &got)
	if got.Doc.Spending != nil {
		t.Errorf("a what-if was saved: %+v", got.Doc)
	}

	req := httptest.NewRequest("PUT", "/api/plan", strings.NewReader(`{"birth_year":1990}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.test")
	w := httptest.NewRecorder()
	security(a.routes()).ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("cross-origin PUT: HTTP %d", w.Code)
	}
}

func TestResolvePlan(t *testing.T) {
	base := planBaseline{Income: 500000, Spending: 300000, Unvested: 4000000, Stock: 1000000, SurplusAccount: "k", Accounts: []planAccount{
		{ID: "chk", Type: "depository", Include: true, Bucket: "cash", Growth: 1, Balance: 100},
		{ID: "k", Type: "investment", Include: true, Bucket: "pretax", Growth: 6, Contribution: 50, Balance: 200, PenaltyFreeAge: 59.5},
		{ID: "ira", Type: "investment", Include: true, Bucket: "roth", Growth: 6, Balance: 300, PenaltyFreeAge: 59.5},
		{ID: "card", Include: false, Bucket: "cash", Balance: -300},
	}, IncomeSources: []planSource{{Name: "PAYROLL", Amount: 4800000}, {Name: "OLD JOB", Amount: 1200000, Stopped: true}}}
	growth, off := 5.0, false
	spend := int64(250000)
	doc := planDoc{BirthYear: 1985, Spending: &spend, Accounts: map[string]planAccountDoc{"k": {Growth: &growth}, "chk": {Include: &off}, "gone": {Include: &off}}}
	jan1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := resolvePlan(doc, base, jan1)
	if in.RetireAge != 65 || in.EndAge != 95 || in.SSAge != 67 || in.SSMonthly != 200000 || in.RetireExtra != 650000 || in.HealthBefore65 != 1500000 || in.Inflation != 3 || in.RetireSpendPct != 100 {
		t.Errorf("defaults = %+v", in)
	}
	// The stopped source is left out of income: $1,000 a month less. Investments without their own growth take
	// the plan's; the surplus goes where the data's moves went.
	if in.Income != 400000 || in.Spending != 250000 || len(in.Accounts) != 2 || in.Accounts[0].Growth != 5 || in.Accounts[0].Contribution != 50 || in.Accounts[1].Growth != 6 || in.SurplusAccount != "k" {
		t.Errorf("resolved = %+v", in)
	}
	if in.Elapsed != 0 {
		t.Errorf("on January 1 nothing of the year is past: %v", in.Elapsed)
	}
	// Counting the stopped source and leaving out the other; a growth for every investment; Extra savings.
	g7 := 7.0
	doc.IncomeSources, doc.Growth, doc.SurplusAccount = map[string]bool{"OLD JOB": true, "PAYROLL": false}, &g7, "extra"
	if in := resolvePlan(doc, base, jan1); in.Income != 500000-400000 || in.Accounts[1].Growth != 7 || in.Accounts[0].Growth != 5 || in.SurplusAccount != "" {
		t.Errorf("owner's sources and growth = %+v", in)
	}
	// Stock pay from the data, only the granted stock at 65% after tax by default.
	if in.Stock != (planStockIn{Yearly: 1000000, Until: "granted", Granted: 2600000, AfterTaxPct: 65}) {
		t.Errorf("stock pay = %+v", in.Stock)
	}
	yearly := int64(300000)
	doc.StockYearly, doc.StockUntil = &yearly, "retire"
	if s := resolvePlan(doc, base, jan1).Stock; s != (planStockIn{Yearly: 300000, Until: "retire", Granted: 2600000, AfterTaxPct: 65}) {
		t.Errorf("owner's stock pay = %+v", s)
	}
	// Social Security: the figure at 67 lowered for years worked (from 22), then for the age it starts.
	for _, c := range []struct{ retire, years int }{{65, 35}, {13 + 22, 13}} {
		doc.RetireAge = &c.retire
		if s := resolvePlan(doc, base, jan1).SS; s.Source != "default" || s.YearsWorked != c.years || s.Worked != ssEstimate(planDefault.SSMonthly, c.years) || s.Monthly != s.Worked {
			t.Errorf("retire at %d: %+v", c.retire, s)
		}
	}
	ssm, at62, at70 := int64(400000), 62, 70
	doc.SSMonthly, doc.SSAge = &ssm, &at62
	retire := 65
	doc.RetireAge = &retire
	if in := resolvePlan(doc, base, jan1); in.SSMonthly != 280000 || in.SS.Source != "owner" || in.SS.AtFRA != 400000 {
		t.Errorf("owner's Social Security at 62: %+v", in.SS)
	}
	doc.SSAge = &at70
	if in := resolvePlan(doc, base, jan1); in.SSMonthly != 496000 {
		t.Errorf("owner's Social Security at 70: %+v", in.SS)
	}
	if e := resolvePlan(doc, base, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)).Elapsed; e != 274.0/365 {
		t.Errorf("October 2 elapsed = %v", e)
	}
}

func TestSSFactor(t *testing.T) {
	for age, want := range map[int]float64{62: 0.70, 63: 0.75, 64: 0.80, 65: 0.8667, 66: 0.9333, 67: 1, 68: 1.08, 69: 1.16, 70: 1.24} {
		if got := ssFactor(age); math.Abs(got-want) > 0.0001 {
			t.Errorf("at %d: %v, want %v", age, got, want)
		}
	}
}

// Market paths: a plan with far more than it needs always lasts, one with nothing never does, and one in between
// lasts in some paths; the same plan gives the same answer every time.
func TestMarketRuns(t *testing.T) {
	in := planCase(46, projAccount{ID: "b", Bucket: "taxable", Balance: 500000000, Growth: 6})
	in.EndAge, in.Spending, in.Inflation = 90, 200000, 3
	if c, low := marketRuns(in); c != 1 || len(low) != 45 {
		t.Errorf("rich: chance %v, %d years", c, len(low))
	}
	in.Accounts[0].Balance = 0
	if c, _ := marketRuns(in); c != 0 {
		t.Errorf("broke: chance %v", c)
	}
	in.Accounts[0].Balance, in.Spending = 60000000, 250000 // $30,000 a year from $600,000: 5%
	c, low := marketRuns(in)
	if c <= 0.1 || c >= 0.9 {
		t.Errorf("borderline: chance %v", c)
	}
	steady, _ := project(in)
	if low[10] >= steady[10].Assets {
		t.Errorf("a poor market (%d) should be below the steady one (%d)", low[10], steady[10].Assets)
	}
	if c2, _ := marketRuns(in); c2 != c {
		t.Errorf("not repeatable: %v then %v", c, c2)
	}
}

func TestSSEstimate(t *testing.T) {
	full := planDefault.SSMonthly
	if got := ssEstimate(full, 35); got != full {
		t.Errorf("35 years = %d", got)
	}
	if got := ssEstimate(full, 50); got != full {
		t.Errorf("past 35 years is capped: %d", got)
	}
	if got := ssEstimate(full, 0); got != 0 {
		t.Errorf("no years = %d", got)
	}
	// Between the straight scale-down ($743) and the full benefit, because the formula favors low earnings.
	if got := ssEstimate(full, 13); got < 118000 || got > 122000 || got%100 != 0 {
		t.Errorf("13 years = %d", got)
	}
	var prev int64
	for y := 0; y <= 40; y++ {
		if got := ssEstimate(full, y); got < prev {
			t.Errorf("%d years = %d, below %d for one fewer", y, got, prev)
		} else {
			prev = got
		}
	}
}

func TestGoals(t *testing.T) {
	a, today := planApp(t)
	// Savings ($38,000 now) was $32,600 six months ago: $900 a month.
	if _, err := a.db.Exec(`INSERT INTO balances(account_id,date,current) VALUES('sav','2026-03-15',3260000)`); err != nil {
		t.Fatal(err)
	}
	var made goalView
	a.call(t, "POST", "/api/goals", map[string]any{"name": "Emergency fund", "target_months": 6, "accounts": []map[string]any{{"account_id": "sav", "pct": 60}}}, 200, &made)
	var bad map[string]string
	a.call(t, "POST", "/api/goals", map[string]any{"name": "House", "target": 8000000, "target_date": "2029-06", "accounts": []map[string]any{{"account_id": "sav", "pct": 50}}}, 400, &bad)
	if !strings.Contains(bad["error"], "40% is still free") {
		t.Errorf("shared account = %q", bad["error"])
	}
	a.call(t, "POST", "/api/goals", map[string]any{"name": "House", "target": 8000000, "target_date": "2029-06", "accounts": []map[string]any{{"account_id": "sav", "pct": 40}, {"account_id": "chk", "pct": 100}}}, 200, nil)
	a.call(t, "POST", "/api/goals", map[string]any{"name": "Trip", "target": 500000, "target_date": "2027-06", "accounts": []map[string]any{{"account_id": "brk", "pct": 10}}}, 200, nil)

	goals, err := a.goals(context.Background(), today)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]goalView{}
	for _, g := range goals {
		by[g.Name] = g
	}
	// 6 months of $3,000 spending; 60% of $38,000.
	if e := by["Emergency fund"]; e.Target != 1800000 || e.Current != 2280000 || !e.Reached || e.Needed != nil {
		t.Errorf("emergency fund = %+v", e)
	}
	// $80,000 by June 2029, 33 months away: (80,000 - 15,200 - 14,000) / 33 = $1,539.40 needed; savings grew
	// 40% of $900 a month and checking has no history, so the pace is $360.
	h := by["House"]
	if h.Current != 1520000+1400000 || h.Needed == nil || *h.Needed != 153940 || h.Pace == nil || *h.Pace != 36000 || h.Reached {
		t.Errorf("house = %+v needed %v pace %v", h, deref(h.Needed), deref(h.Pace))
	}
	if tr := by["Trip"]; !tr.Reached { // 10% of $124,000
		t.Errorf("trip = %+v", tr)
	}

	// The emergency fund's target follows the plan's spending.
	a.call(t, "PUT", "/api/plan", map[string]any{"birth_year": 1985, "spending_monthly": 400000}, 200, nil)
	goals, _ = a.goals(context.Background(), today)
	for _, g := range goals {
		if g.Name == "Emergency fund" && g.Target != 2400000 {
			t.Errorf("target after spending changed = %d", g.Target)
		}
	}

	// Editing a goal may keep its own share of an account; deleting frees it.
	a.call(t, "PUT", fmt.Sprintf("/api/goals/%d", made.ID), map[string]any{"name": "Emergency fund", "target_months": 3, "accounts": []map[string]any{{"account_id": "sav", "pct": 60}}}, 200, nil)
	a.call(t, "DELETE", fmt.Sprintf("/api/goals/%d", made.ID), nil, 200, nil)
	a.call(t, "POST", "/api/goals", map[string]any{"name": "Car", "target": 100, "accounts": []map[string]any{{"account_id": "sav", "pct": 60}}}, 200, nil)
	a.call(t, "POST", "/api/goals", map[string]any{"name": "", "target": 100}, 400, nil)
	a.call(t, "POST", "/api/goals", map[string]any{"name": "Both", "target": 100, "target_months": 2}, 400, nil)
}

// Moves between cash and investment accounts that matchTransfers missed (too far apart) are left out of the
// baseline, on both sides.
func TestPlanPaired(t *testing.T) {
	a, today := planApp(t)
	if _, err := a.db.Exec(`INSERT INTO accounts(id,item_id,name,guessed_type,current) VALUES('stock','item','Equity Awards','investment',500000);
		INSERT INTO transactions(id,account_id,date,amount,name,user_category) VALUES
			('xfer-out','chk','2026-06-20',-50000,'ONLINE TRANSFER','Transfer'),
			('xfer-in','brk','2026-06-28',50000,'FUNDS RECEIVED',''),
			('proceeds-out','stock','2026-07-01',-1000000,'WIRE TO BANK',''),
			('proceeds-in','chk','2026-07-07',1000000,'WIRE FROM BROKER',''),
			('refund','chk','2026-07-09',50000,'REFUND',''),
			('far-out','chk','2026-08-01',-70000,'ONLINE TRANSFER','Transfer'),
			('far-in','brk','2026-08-12',70000,'FUNDS RECEIVED','')`); err != nil {
		t.Fatal(err)
	}
	b, err := a.planBaseline(context.Background(), today)
	if err != nil {
		t.Fatal(err)
	}
	// $5,000 payroll, $500 refund and $3,000 spending a month stay; the paired $10,000 proceeds are not income.
	if b.Income != 500000+50000/5 || b.Spending != 300000 || b.Paired != (50000+1000000)*12/5 {
		t.Errorf("income %d, spending %d, paired %d", b.Income, b.Spending, b.Paired)
	}
	for _, ac := range b.Accounts {
		// The 11-day gap is too wide: that deposit stays a contribution.
		if want := int64(70000 * 12 / 5); ac.ID == "brk" && ac.Deposits != want {
			t.Errorf("brk deposits = %d, want %d", ac.Deposits, want)
		}
	}
	// A deposit with no investment partner is not paired; with two candidates the closer one wins, once each.
	if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name) VALUES
		('o1','chk','2026-06-01',-9900,'X'),('o2','chk','2026-06-02',-9900,'X'),('i1','brk','2026-06-03',9900,'X'),('i2','brk','2026-06-09',9900,'X')`); err != nil {
		t.Fatal(err)
	}
	p, moved, err := a.planPairs(context.Background(), "2026-06-01", "2026-07-01")
	if err != nil {
		t.Fatal(err)
	}
	if !p["o1"] || !p["i1"] || !p["o2"] || !p["i2"] || p["refund"] || moved != 50000+2*9900 {
		t.Errorf("pairs = %v, moved %d", p, moved)
	}
}
