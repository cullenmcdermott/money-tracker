package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDetectRecurring(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse(time.DateOnly, s); return v }
	series := func(amts []int64, dates ...string) []charge {
		cs := make([]charge, len(dates))
		for i, s := range dates {
			cs[i] = charge{d(s), amts[i%len(amts)]}
		}
		return cs
	}
	today := d("2026-09-20")
	cases := []struct {
		name    string
		cs      []charge
		ok      bool
		cadence string
		active  bool
		prev    int64 // 0 = no price change
	}{
		{"monthly, price went up", series([]int64{1549, 1549, 1799}, "2026-07-05", "2026-08-05", "2026-09-05"), true, "monthly", true, 1549},
		{"monthly, month-end drift", series([]int64{999}, "2026-06-30", "2026-07-31", "2026-08-29", "2026-09-30"), true, "monthly", true, 0},
		{"monthly, stopped", series([]int64{999}, "2026-03-01", "2026-04-01", "2026-05-01"), true, "monthly", false, 0},
		{"weekly", series([]int64{2500, 2600}, "2026-08-30", "2026-09-06", "2026-09-13", "2026-09-20"), true, "weekly", true, 0},
		{"yearly from two charges", series([]int64{13900}, "2025-03-10", "2026-03-12"), true, "yearly", true, 0},
		{"two visits a year apart", series([]int64{5500, 6600}, "2025-02-18", "2026-02-23"), false, "", false, 0},
		{"two monthly charges are not enough", series([]int64{999}, "2026-08-05", "2026-09-05"), false, "", false, 0},
		{"irregular dates", series([]int64{999}, "2026-06-01", "2026-06-09", "2026-07-30", "2026-08-02"), false, "", false, 0},
		{"wildly varying amounts", series([]int64{1000, 9000, 300}, "2026-06-05", "2026-07-05", "2026-08-05"), false, "", false, 0},
	}
	for _, c := range cases {
		r, ok := detectRecurring(c.cs, today, "subscription")
		if ok != c.ok || r.Cadence != c.cadence || (ok && r.Active != c.active) {
			t.Errorf("%s: ok=%v cadence=%q active=%v", c.name, ok, r.Cadence, r.Active)
		}
		if c.prev != 0 && r.Monthly != r.Amount {
			t.Errorf("%s: monthly=%d, want the new price %d", c.name, r.Monthly, r.Amount)
		}
		if got := r.Previous; (got == nil) != (c.prev == 0) || (got != nil && *got != c.prev) {
			t.Errorf("%s: previous=%v, want %d", c.name, got, c.prev)
		}
	}
	if r, _ := detectRecurring(series([]int64{13900}, "2025-03-10", "2026-03-12"), today, "subscription"); r.Monthly != 1158 || r.Next != "2027-03-12" {
		t.Errorf("yearly monthly=%d next=%s", r.Monthly, r.Next)
	}
}

func TestDetectRecurringKinds(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse(time.DateOnly, s); return v }
	today := d("2026-09-20")
	utility := []charge{{d("2026-05-15"), 6200}, {d("2026-06-15"), 14000}, {d("2026-07-15"), 11800}, {d("2026-08-15"), 7100}}
	if r, ok := detectRecurring(utility, today, "bill"); !ok || r.Kind != "bill" || r.Cadence != "monthly" || r.Low != 6200 || r.High != 14000 || r.Typical != 11800 || r.Monthly != 11800 || r.Previous != nil {
		t.Errorf("swinging utility = %+v, %v", r, ok)
	}
	if _, ok := detectRecurring(utility, today, "subscription"); ok {
		t.Error("swinging amounts detected as a subscription")
	}
	// A bill's range covers the last 12 months only.
	old := append([]charge{{d("2025-08-15"), 30000}}, utility...)
	for i := 1; i < 9; i++ {
		old = slices.Insert(old, i, charge{d("2025-08-15").AddDate(0, i, 0), 9000})
	}
	if r, ok := detectRecurring(old, today, "bill"); !ok || r.High != 14000 {
		t.Errorf("bill range ignores old charges: %+v, %v", r, ok)
	}

	monthly := func(last string) []charge {
		l := d(last)
		return []charge{{l.AddDate(0, -2, 0), 999}, {l.AddDate(0, -1, 0), 999}, {l, 999}}
	}
	if r, _ := detectRecurring(monthly("2026-09-10"), today, "subscription"); !r.New {
		t.Error("third monthly charge 10 days ago is not New")
	}
	if r, _ := detectRecurring(monthly("2026-08-11"), today, "subscription"); r.New {
		t.Error("third monthly charge 40 days ago is New")
	}
	if r, _ := detectRecurring([]charge{{d("2025-09-01"), 9900}, {d("2026-09-01"), 9900}}, today, "subscription"); !r.New {
		t.Error("second yearly charge 19 days ago is not New")
	}

	every := func(first string, days, n int, amt int64) []charge {
		cs := make([]charge, n)
		for i := range cs {
			cs[i] = charge{d(first).AddDate(0, 0, i*days), amt}
		}
		return cs
	}
	pay := every("2026-01-02", 14, 19, 310000) // the last is 2026-09-04
	if r, ok := detectRecurring(pay, today, "income"); !ok || r.Kind != "income" || r.Cadence != "every 2 weeks" || r.Monthly < 660000 || r.Monthly > 680000 {
		t.Errorf("biweekly paycheck = %+v, %v", r, ok)
	}
	var semi []charge
	for m := time.January; m <= time.August; m++ {
		semi = append(semi, charge{time.Date(2026, m, 15, 0, 0, 0, 0, time.UTC), 300000}, charge{time.Date(2026, m+1, 0, 0, 0, 0, 0, time.UTC), 300000})
	}
	if r, ok := detectRecurring(semi, today, "income"); !ok || r.Cadence != "twice a month" {
		t.Errorf("semi-monthly paycheck = %+v, %v", r, ok)
	}
	bonus := slices.Clone(pay)
	bonus[len(bonus)-2].amount = 900000
	if r, ok := detectRecurring(bonus, today, "income"); !ok || r.Monthly <= 680000 || r.Previous != nil {
		t.Errorf("paycheck with a bonus = %+v, %v", r, ok)
	}
}

func TestRecurringEndpoint(t *testing.T) {
	a := categoryApp(t)
	// Two raw keys merged into one merchant form one monthly series; a transfer and a deposit on the same key don't count.
	_, err := a.db.Exec(`INSERT INTO merchants(key,display_name) VALUES('netflix','Netflix'),('netflix.com','Netflix.com');
		UPDATE merchants SET merged_into='netflix' WHERE key='netflix.com';
		INSERT INTO transactions(id,account_id,date,amount,name,merchant_key,user_category) VALUES
		('n1','a','2026-06-12',-1549,'NETFLIX','netflix','Subscriptions'),
		('n2','a','2026-07-12',-1549,'NETFLIX.COM','netflix.com','Subscriptions'),
		('n3','a','2026-08-12',-1549,'NETFLIX','netflix','Subscriptions'),
		('refund','a','2026-08-20',1549,'NETFLIX','netflix',''),
		('x1','a','2026-06-01',-5000,'SAVINGS','savings',''),
		('x2','a','2026-07-01',-5000,'SAVINGS','savings',''),
		('x3','a','2026-08-01',-5000,'SAVINGS','savings',''),
		('m1','a','2026-06-02',-215000,'LAKEVIEW MORTGAGE PYMT 5555','lakeview home loans','Housing'),
		('m2','a','2026-07-02',-215000,'LAKEVIEW MORTGAGE PYMT 5555','lakeview home loans','Housing'),
		('m3','a','2026-08-04',-215000,'LAKEVIEW MORTGAGE PYMT 5555','lakeview mortgage payment','Housing'),
		('c1','a','2026-06-20',-100,'CHECK # 1','check one',''),
		('c2','a','2026-07-20',-100,'CHECK # 2','check two',''),
		('c3','a','2026-08-20',-100,'CHECK # 3','check three','')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE transactions SET transfer_id='elsewhere' WHERE id LIKE 'x%'`); err != nil {
		t.Fatal(err)
	}
	got, err := a.recurring(context.Background(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	// One description under two merchant keys (Monarch's name, then ours) is one bill; a one-word description
	// ("check") never joins merchants, so the three checks stay three single charges.
	if len(got) != 2 || got[0].Key != "lakeview mortgage payment" || got[0].Count != 3 || !got[0].Active {
		t.Fatalf("recurring = %+v", got)
	}
	got = got[1:]
	if len(got) != 1 || got[0].Key != "netflix" || got[0].Name != "Netflix" || got[0].Count != 3 || got[0].Category != "Subscriptions" || got[0].Next != "2026-09-11" || !got[0].Active {
		t.Fatalf("recurring = %+v", got)
	}
	var viaHTTP []recurring
	a.call(t, "GET", "/api/recurring", nil, 200, &viaHTTP)
}

func TestRecurringKindsIncomeAndMarks(t *testing.T) {
	a := categoryApp(t)
	today := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	_, err := a.db.Exec(`INSERT INTO merchants(key,display_name) VALUES('stream','Stream'),('stream.tv','Stream.tv');
		UPDATE merchants SET merged_into='stream' WHERE key='stream.tv';
		INSERT INTO transactions(id,account_id,date,amount,name,merchant_key,user_category) VALUES
		('s1','a','2026-05-12',-1549,'STREAM','stream.tv','Subscriptions'),
		('s2','a','2026-06-12',-1549,'STREAM','stream.tv','Subscriptions'),
		('s3','a','2026-07-12',-1549,'STREAM','stream','Subscriptions'),
		('e1','a','2026-05-20',-6200,'EXAMPLE POWER','example power','Utilities'),
		('e2','a','2026-06-20',-14000,'EXAMPLE POWER','example power','Utilities'),
		('e3','a','2026-07-20',-11800,'EXAMPLE POWER','example power','Utilities'),
		('e4','a','2026-08-20',-7100,'EXAMPLE POWER','example power','Utilities'),
		('g1','a','2026-06-03',-4000,'GYM','gym','Subscriptions'),
		('g2','a','2026-07-03',-4000,'GYM','gym','Subscriptions'),
		('g3','a','2026-08-03',-4000,'GYM','gym','Subscriptions')`)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 { // a paycheck every 14 days, plus a refund that is not income
		if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,merchant_key,user_category) VALUES($1,'a',$2,310000,'ACME PAYROLL','acme payroll','Income')`,
			fmt.Sprint("p", i), today.AddDate(0, 0, -6-14*i).Format(time.DateOnly)); err != nil {
			t.Fatal(err)
		}
	}
	// A second household paycheck, two days after each of the first. An import filed one deposit from each
	// employer under a generic "paycheck" merchant; income never joins on descriptions, so they stay two series.
	for i := range 12 {
		if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,merchant_key,user_category) VALUES($1,'a',$2,180000,'EXAMPLE CLINIC PAYROLL','example clinic','Income')`,
			fmt.Sprint("q", i), today.AddDate(0, 0, -4-14*i).Format(time.DateOnly)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,merchant_key,user_category) VALUES('r','a','2026-08-01',1549,'STREAM','stream',''),
		('i1','a','2025-01-03',310000,'ACME PAYROLL','paycheck','Income'),('i2','a','2025-01-05',180000,'EXAMPLE CLINIC PAYROLL','paycheck','Income')`); err != nil {
		t.Fatal(err)
	}
	get := func() map[string]recurring {
		t.Helper()
		got, err := a.recurring(context.Background(), today)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]recurring{}
		for _, r := range got {
			m[r.Key] = r
		}
		return m
	}
	m := get()
	if r := m["example power"]; r.Kind != "bill" || r.Low != 6200 || r.High != 14000 {
		t.Errorf("utility = %+v", r)
	}
	if r := m["acme payroll"]; r.Kind != "income" || r.Cadence != "every 2 weeks" || r.Count != 12 {
		t.Errorf("paycheck = %+v", r)
	}
	if r := m["stream"]; r.Kind != "subscription" || r.Count != 3 || r.Mark != "" {
		t.Errorf("stream = %+v", r)
	}
	if r := m["example clinic"]; r.Kind != "income" || r.Count != 12 {
		t.Errorf("second paycheck = %+v", r)
	}
	if len(m) != 5 {
		t.Errorf("recurring = %+v", m)
	}
	if _, err := a.db.Exec(`UPDATE transactions SET user_category='Utilities' WHERE merchant_key='gym'`); err != nil {
		t.Fatal(err)
	}
	if r := get()["gym"]; r.Kind != "bill" {
		t.Errorf("recategorized gym kind = %q", r.Kind)
	}

	// A mark on the merged-away key still matches the group.
	a.call(t, "PUT", "/api/recurring/stream.tv/mark", map[string]string{"state": "cancelled"}, 200, nil)
	a.call(t, "PUT", "/api/recurring/gym/mark", map[string]string{"state": "hidden"}, 200, nil)
	a.call(t, "PUT", "/api/recurring/gym/mark", map[string]string{"state": "bogus"}, 400, nil)
	if _, err := a.db.Exec(`UPDATE recurring_marks SET marked_at='2026-07-15'`); err != nil {
		t.Fatal(err)
	}
	m = get()
	if r := m["stream"]; r.Mark != "cancelled" || r.MarkKey != "stream.tv" || r.ChargedAfterCancel {
		t.Errorf("cancelled, no later charge = %+v", r)
	}
	if r := m["gym"]; r.Mark != "hidden" {
		t.Errorf("hidden = %+v", r)
	}
	if _, err := a.db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,merchant_key,user_category) VALUES('s4','a','2026-08-12',-1549,'STREAM','stream','Subscriptions')`); err != nil {
		t.Fatal(err)
	}
	if r := get()["stream"]; r.Mark != "cancelled" || !r.ChargedAfterCancel {
		t.Errorf("charged after cancelling = %+v", r)
	}
	a.call(t, "DELETE", "/api/recurring/stream.tv/mark", nil, 200, nil)
	if r := get()["stream"]; r.Mark != "" || r.ChargedAfterCancel {
		t.Errorf("after clearing = %+v", r)
	}

	// Both routes sit behind the CSRF check.
	for _, method := range []string{"PUT", "DELETE"} {
		req := httptest.NewRequest(method, "/api/recurring/gym/mark", strings.NewReader(`{"state":"cancelled"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://evil.test")
		w := httptest.NewRecorder()
		security(a.routes()).ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("cross-origin %s: HTTP %d", method, w.Code)
		}
	}
	if r := get()["gym"]; r.Mark != "hidden" {
		t.Errorf("cross-origin request changed the mark: %+v", r)
	}
}
