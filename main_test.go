package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMonthly(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO items(id) VALUES('item'); INSERT INTO accounts(id,item_id,name,guessed_type) VALUES('account','item','Checking','depository')`)
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id, date string
		amount   int
		pending  int
		transfer any // transfer_id: paired transfers are excluded from cash flow
	}{
		{"income", "2026-08-01", 500000, 0, nil},
		{"food", "2026-08-02", -1999, 0, nil},
		{"transfer", "2026-08-03", -10000, 0, "income"},
		{"payment", "2026-08-04", -5000, 0, "income"},
		{"pending", "2026-08-05", -999, 1, nil},
		{"second", "2026-09-01", -3000, 0, nil},
	}
	for _, row := range rows {
		_, err = db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,pending,transfer_id) VALUES($1,'account',$2,$3,$4,$5,$6)`, row.id, row.date, row.amount, row.id, row.pending != 0, row.transfer)
		if err != nil {
			t.Fatal(err)
		}
	}
	a := app{db: db}
	got, err := a.monthly(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["month"] != "2026-08" || got[0]["income"] != int64(500000) || got[0]["expense"] != int64(1999) || got[1]["month"] != "2026-09" || got[1]["income"] != int64(0) || got[1]["expense"] != int64(3000) {
		t.Fatalf("monthly = %#v", got)
	}
}

func TestMigrateFreshAndIdempotent(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	files, _ := fs.Glob(migrations, "migrations/*.sql")
	applied := func() (n int) {
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := applied(); n != len(files) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(files))
	}
	var tables int
	db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name IN ('items','accounts','balances','transactions')`).Scan(&tables)
	if tables != 4 {
		t.Fatalf("found %d of 4 tables", tables)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := applied(); n != len(files) {
		t.Fatalf("schema_migrations changed to %d rows", n)
	}
}

func TestTransactionFiltersAndIncomeSummary(t *testing.T) {
	a := categoryApp(t)
	a.insertTx(t, "pay", "ACME PAYROLL", "Acme", 400000, 0, nil)
	a.insertTx(t, "pay2", "ACME PAYROLL", "Acme", 400000, 0, nil)
	a.insertTx(t, "int", "Interest Paid", "", 1000, 0, nil)
	a.insertTx(t, "cafe", "POS 123", "Cafe Nero", -250, 0, nil)
	a.insertTx(t, "food", "Grocer_100%", "", -900, 0, nil)
	a.insertTx(t, "pend", "Cafe pending", "", -100, 1, nil)
	a.insertTx(t, "xfer", "Move", "", -5000, 0, "other")
	if _, err := a.db.Exec(`UPDATE transactions SET user_category=CASE WHEN id='food' THEN 'Groceries' ELSE 'Income' END WHERE id IN ('food','pay','pay2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name) VALUES('old','b','2025-12-31',-700,'Old thing')`); err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID        string `json:"id"`
		AccountID string `json:"account_id"`
		Transfer  bool   `json:"transfer"`
	}
	ids := func(query string) (string, string) {
		t.Helper()
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest("GET", "/api/transactions?"+query, nil))
		if w.Code != 200 {
			t.Fatalf("%s: HTTP %d %s", query, w.Code, w.Body.String())
		}
		var rows []row
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, r.ID)
		}
		return strings.Join(got, ","), w.Header().Get("X-Total-Count")
	}
	for query, want := range map[string]string{
		"month=2025":                     "old",
		"month=2026-08&q=CAFE":           "pend,cafe",
		"month=2026-08&category=":        "int,cafe",
		"category=Groceries":             "food",
		"flow=in&month=2026":             "pay2,pay,int",
		"flow=in&q=acme":                 "pay2,pay",
		"account_id=b":                   "old",
		"q=grocer_1":                     "food",
		"q=%25":                          "food",
		"month=2026-08&flow=out&limit=1": "food",
		"month=2025-12..2026-08&flow=in": "pay2,pay,int",
	} {
		if got, _ := ids(query); got != want {
			t.Errorf("%s: got %q, want %q", query, got, want)
		}
	}
	if got, total := ids("month=2026-08&limit=2&offset=2"); total != "7" || strings.Count(got, ",") != 1 {
		t.Errorf("paging: got %q of %s", got, total)
	}
	var withTransfer []row
	a.call(t, "GET", "/api/transactions?q=move", nil, 200, &withTransfer)
	if len(withTransfer) != 1 || !withTransfer[0].Transfer || withTransfer[0].AccountID != "a" {
		t.Errorf("transfer row = %#v", withTransfer)
	}
	for _, bad := range []string{"month=2026-13", "month=26", "month=2026-08..2026-07", "month=2026..2026-08", "flow=sideways", "offset=-1"} {
		a.call(t, "GET", "/api/transactions?"+bad, nil, 400, nil)
	}

	var income []struct {
		Source string `json:"source"`
		Amount int64  `json:"amount"`
	}
	a.call(t, "GET", "/api/summary/income?month=2026", nil, 200, &income)
	if len(income) != 2 || income[0].Source != "Acme" || income[0].Amount != 800000 || income[1].Source != "Interest Paid" {
		t.Errorf("income = %#v", income)
	}
	a.call(t, "GET", "/api/summary/income", nil, 400, nil)
	var cats []struct {
		Category string `json:"category"`
		Amount   int64  `json:"amount"`
	}
	a.call(t, "GET", "/api/summary/categories?month=2026", nil, 200, &cats)
	if len(cats) != 2 || cats[0].Category != "Groceries" && cats[0].Category != "" {
		t.Errorf("year categories = %#v", cats)
	}
}

func TestCashflowCountsOnlyCashAccountsAndReportsInvested(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO items(id) VALUES('item');
		INSERT INTO accounts(id,item_id,name,guessed_type) VALUES('chk','item','Checking','depository'),('card','item','Card','credit'),
			('brk','item','Brokerage','investment'),('stock','item','Stock Plan','other'),('mort','item','Mortgage','loan')`); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		id, account, date string
		amount            int
		transfer          any
	}{
		{"pay", "chk", "2026-08-01", 500000, nil},
		{"food", "card", "2026-08-02", -2000, nil},
		{"to-brk", "chk", "2026-08-03", -100000, "brk-in"}, // checking -> brokerage: invested
		{"brk-in", "brk", "2026-08-03", 100000, "to-brk"},
		{"buy", "brk", "2026-08-04", -100000, nil}, // fund purchase inside the brokerage: not spending
		{"div", "brk", "2026-08-05", 3000, nil},    // dividend: not income
		{"vest", "stock", "2026-08-06", 1800000, nil},
		{"principal", "mort", "2026-08-07", 56000, nil},
		{"sale", "stock", "2026-09-01", -40000, "back"}, // money back to checking: negative invested
		{"back", "chk", "2026-09-01", 40000, "sale"},
		{"CONTOSO BUY INVESTMENT", "chk", "2026-08-10", -25000, nil}, // unpaired, marked Transfer below: invested, not spending
	} {
		if _, err := db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,transfer_id) VALUES($1,$2,$3,$4,$1,$5)`, r.id, r.account, r.date, r.amount, r.transfer); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE accounts SET institution='Contoso Investments' WHERE id='brk'; UPDATE transactions SET user_category='Transfer' WHERE id='CONTOSO BUY INVESTMENT'`); err != nil {
		t.Fatal(err)
	}
	got, err := (&app{db: db}).monthly(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if want := `[{"expense":2000,"income":500000,"invested":125000,"month":"2026-08"}]`; string(b) != want {
		t.Errorf("monthly = %s, want %s (September has no cash flow rows, only the withdrawal)", b, want)
	}
	invested, err := (&app{db: db}).investedByMonth(context.Background())
	if err != nil || invested["2026-09"] != -40000 {
		t.Errorf("September invested = %d, %v; want -40000", invested["2026-09"], err)
	}
}
