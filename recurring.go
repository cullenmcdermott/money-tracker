package main

import (
	"context"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Recurring charges are detected at read time from cash-flow outflows grouped by resolved merchant:
// no table, so renames, merges and new syncs show up immediately.

type cadence struct {
	name     string
	lo, hi   int     // accepted days between charges
	perMonth float64 // charges per month, to normalize the monthly cost
}

var cadences = []cadence{
	{"weekly", 6, 8, 52.0 / 12},
	{"every 2 weeks", 13, 16, 26.0 / 12},
	{"monthly", 26, 35, 1},
	{"quarterly", 84, 98, 1.0 / 3},
	{"yearly", 350, 380, 1.0 / 12},
}

type charge struct {
	date   time.Time
	amount int64 // positive cents spent
}

type recurring struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Cadence  string `json:"cadence"`
	Amount   int64  `json:"amount"`   // latest charge
	Monthly  int64  `json:"monthly"`  // typical charge normalized to a month
	Previous *int64 `json:"previous"` // the steady amount before the latest charge changed it, else null
	Last     string `json:"last"`
	Next     string `json:"next"`
	Count    int    `json:"count"`
	Active   bool   `json:"active"`
}

// detectRecurring decides whether one merchant's charges (sorted by date) repeat on a schedule.
// ponytail: fixed day bands and a ±30% amount tolerance; tune here if real data shows misses or false hits
// (irregular bills like utilities that swing more than 30% drop out).
func detectRecurring(cs []charge, today time.Time) (recurring, bool) {
	if len(cs) < 2 {
		return recurring{}, false
	}
	gaps := make([]int, len(cs)-1)
	for i := range gaps {
		gaps[i] = int(cs[i+1].date.Sub(cs[i].date).Hours() / 24)
	}
	med := slices.Clone(gaps)
	slices.Sort(med)
	var c *cadence
	for i := range cadences {
		if g := med[len(med)/2]; g >= cadences[i].lo && g <= cadences[i].hi {
			c = &cadences[i]
		}
	}
	// Two charges only prove a yearly schedule; anything more frequent needs a third to show a pattern.
	if c == nil || (len(cs) < 3 && c.lo < 350) {
		return recurring{}, false
	}
	fit := 0
	for _, g := range gaps {
		if g >= c.lo && g <= c.hi {
			fit++
		}
	}
	amts := make([]int64, len(cs))
	for i, x := range cs {
		amts[i] = x.amount
	}
	sorted := slices.Clone(amts)
	slices.Sort(sorted)
	typical := sorted[len(sorted)/2]
	near := 0
	for _, a := range amts {
		if math.Abs(float64(a-typical)) <= 0.3*float64(typical) {
			near++
		}
	}
	if fit*3 < len(gaps)*2 || near*3 < len(amts)*2 {
		return recurring{}, false
	}
	// Two charges a year apart at different prices are more likely a repeat visit (a restaurant, a rental)
	// than a subscription, which bills the same amount.
	if len(cs) == 2 && math.Abs(float64(amts[1]-amts[0])) > 0.05*float64(amts[0]) {
		return recurring{}, false
	}
	last := cs[len(cs)-1]
	period := (c.lo + c.hi) / 2
	next := last.date.AddDate(0, 0, period)
	r := recurring{
		Cadence: c.name, Amount: last.amount, Monthly: int64(math.Round(float64(typical) * c.perMonth)),
		Last: last.date.Format(time.DateOnly), Next: next.Format(time.DateOnly), Count: len(cs),
		// Missed by more than half a period (plus slack for weekends and posting delays): probably cancelled.
		Active: !today.After(next.AddDate(0, 0, period/2+3)),
	}
	// A price change: the two charges before the latest agree, and the latest differs from them.
	if n := len(amts); n >= 3 && amts[n-2] == amts[n-3] && amts[n-1] != amts[n-2] {
		r.Previous = &amts[n-2]
		r.Monthly = int64(math.Round(float64(last.amount) * c.perMonth)) // the new price is what it costs from now on
	}
	return r, true
}

func (a *app) recurring(ctx context.Context, today time.Time) ([]recurring, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT COALESCE(mr.root,c.merchant_key),COALESCE(mr.display_name,NULLIF(c.merchant,''),c.name),c.name,c.effective_category,c.date,-c.amount
		FROM cashflow c LEFT JOIN merchant_roots mr ON mr.key=c.merchant_key
		WHERE c.amount<0 AND c.merchant_key!='' ORDER BY c.date,c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		key, name, desc, category string
		c                         charge
	}
	var all []row
	for rows.Next() {
		var r row
		var d string
		if err := rows.Scan(&r.key, &r.name, &r.desc, &r.category, &d, &r.c.amount); err != nil {
			return nil, err
		}
		if r.c.date, err = time.Parse(time.DateOnly, d); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The same bank description can carry different merchant keys (Monarch's merchant name on imported rows,
	// ours on synced ones), which would split one bill in two at the import date. Join merchants that share a
	// specific description: two or more words, used by at most three merchants (so "CHECK #123" or a generic
	// "ZELLE PAYMENT" never chains unrelated merchants together).
	parent := map[string]string{}
	var find func(string) string
	find = func(k string) string {
		if p, ok := parent[k]; ok && p != k {
			parent[k] = find(p)
			return parent[k]
		}
		return k
	}
	byDesc := map[string]map[string]bool{}
	for _, r := range all {
		if d, _ := cleanMerchant(r.desc); strings.Contains(d, " ") {
			if byDesc[d] == nil {
				byDesc[d] = map[string]bool{}
			}
			byDesc[d][r.key] = true
		}
	}
	for _, keys := range byDesc {
		if len(keys) < 2 || len(keys) > 3 {
			continue
		}
		var first string
		for k := range keys {
			if first == "" {
				first = k
			} else if a, b := find(first), find(k); a != b {
				parent[b] = a
			}
		}
	}
	groups := map[string][]row{}
	for _, r := range all {
		g := find(r.key)
		groups[g] = append(groups[g], r)
	}
	out := make([]recurring, 0)
	for _, rs := range groups {
		cs := make([]charge, len(rs))
		for i, r := range rs {
			cs[i] = r.c
		}
		if r, ok := detectRecurring(cs, today); ok {
			last := rs[len(rs)-1] // the latest charge's merchant, name and category win
			r.Key, r.Name, r.Category = last.key, last.name, last.category
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(x, y recurring) int {
		if x.Monthly != y.Monthly {
			return int(y.Monthly - x.Monthly)
		}
		return strings.Compare(x.Key, y.Key)
	})
	return out, nil
}

func (a *app) recurringRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/recurring", func(w http.ResponseWriter, r *http.Request) {
		v, err := a.recurring(r.Context(), time.Now())
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, v)
	})
}
