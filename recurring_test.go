package main

import (
	"context"
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
		r, ok := detectRecurring(c.cs, today)
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
	if r, _ := detectRecurring(series([]int64{13900}, "2025-03-10", "2026-03-12"), today); r.Monthly != 1158 || r.Next != "2027-03-12" {
		t.Errorf("yearly monthly=%d next=%s", r.Monthly, r.Next)
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
