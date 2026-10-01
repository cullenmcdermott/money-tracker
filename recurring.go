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

// Paychecks come every 2 weeks or twice a month, whose gaps (12 to 19 days) straddle the "every 2 weeks" band,
// so income treats them as one cadence, named by the median gap.
var incomeCadences = []cadence{cadences[0], {"every 2 weeks", 12, 19, 26.0 / 12}, cadences[2], cadences[3], cadences[4]}

// Bills vary in amount (utilities swing with the season), so they're detected on interval alone. Everything else
// is a subscription. Kind follows the latest charge's category; to move one, recategorize it.
var billCategories = map[string]bool{"Housing": true, "Utilities": true, "Insurance": true, "Transportation": true}

type charge struct {
	date   time.Time
	amount int64 // positive cents spent
}

type recurring struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Kind     string `json:"kind"` // subscription, bill or income
	Cadence  string `json:"cadence"`
	Amount   int64  `json:"amount"`   // latest charge
	Typical  int64  `json:"typical"`  // median charge
	Monthly  int64  `json:"monthly"`  // typical charge normalized to a month; for income, the last 6 months' average
	Previous *int64 `json:"previous"` // the steady amount before the latest charge changed it, else null
	Low      int64  `json:"low"`      // bills: the smallest and largest charge in the last 12 months
	High     int64  `json:"high"`
	Last     string `json:"last"`
	Next     string `json:"next"`
	Count    int    `json:"count"`
	Active   bool   `json:"active"`
	New      bool   `json:"new"`                // became recurring in the last 30 days
	Mark     string `json:"mark"`               // '', cancelled or hidden: the owner's mark on any key in the group
	MarkKey  string `json:"mark_key,omitempty"` // the key holding the mark, to clear it
	// Cancelled, but billed again after the mark.
	ChargedAfterCancel bool `json:"charged_after_cancel"`
}

// detectRecurring decides whether one merchant's charges (sorted by date) repeat on a schedule. kind is
// subscription, bill (no amount checks) or income (deposits, with the wider paycheck band).
// ponytail: fixed day bands and a ±30% amount tolerance; tune here if real data shows misses or false hits.
func detectRecurring(cs []charge, today time.Time, kind string) (recurring, bool) {
	if len(cs) < 2 {
		return recurring{}, false
	}
	gaps := make([]int, len(cs)-1)
	for i := range gaps {
		gaps[i] = int(cs[i+1].date.Sub(cs[i].date).Hours() / 24)
	}
	med := slices.Clone(gaps)
	slices.Sort(med)
	bands := cadences
	if kind == "income" {
		bands = incomeCadences
	}
	var c *cadence
	for i := range bands {
		if g := med[len(med)/2]; g >= bands[i].lo && g <= bands[i].hi {
			c = &bands[i]
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
	bill := kind == "bill"
	if fit*3 < len(gaps)*2 || (!bill && near*3 < len(amts)*2) {
		return recurring{}, false
	}
	// Two charges a year apart at different prices are more likely a repeat visit (a restaurant, a rental)
	// than a subscription, which bills the same amount.
	if !bill && len(cs) == 2 && math.Abs(float64(amts[1]-amts[0])) > 0.05*float64(amts[0]) {
		return recurring{}, false
	}
	last := cs[len(cs)-1]
	period := (c.lo + c.hi) / 2
	next := last.date.AddDate(0, 0, period)
	minCount := 3
	if c.lo >= 350 {
		minCount = 2
	}
	r := recurring{
		Kind: kind, Cadence: c.name, Amount: last.amount, Typical: typical, New: !cs[minCount-1].date.Before(today.AddDate(0, 0, -30)), Monthly: int64(math.Round(float64(typical) * c.perMonth)),
		Last: last.date.Format(time.DateOnly), Next: next.Format(time.DateOnly), Count: len(cs),
		// Missed by more than half a period (plus slack for weekends and posting delays): probably cancelled.
		Active: !today.After(next.AddDate(0, 0, period/2+3)),
	}
	switch kind {
	case "bill":
		r.Low, r.High = math.MaxInt64, 0
		for _, x := range cs {
			if x.date.After(today.AddDate(-1, 0, 0)) {
				r.Low, r.High = min(r.Low, x.amount), max(r.High, x.amount)
			}
		}
		if r.High == 0 {
			r.Low = 0 // nothing in the last year: an old bill under Not seen lately
		}
		return r, true
	case "income":
		if c.name == "every 2 weeks" && med[len(med)/2] > 14 {
			r.Cadence = "twice a month"
		}
		// Semi-monthly pay doesn't fit perMonth, and bonuses count, so average what actually came in.
		from := today.AddDate(0, -6, 0)
		if cs[0].date.After(from) {
			from = cs[0].date
		}
		var sum int64
		for _, x := range cs {
			if !x.date.Before(from) {
				sum += x.amount
			}
		}
		r.Monthly = int64(math.Round(float64(sum) / max(today.Sub(from).Hours()/24/(365.25/12), 1)))
		return r, true
	}
	// A price change: the two charges before the latest agree, and the latest differs from them.
	if n := len(amts); n >= 3 && amts[n-2] == amts[n-3] && amts[n-1] != amts[n-2] {
		r.Previous = &amts[n-2]
		r.Monthly = int64(math.Round(float64(last.amount) * c.perMonth)) // the new price is what it costs from now on
	}
	return r, true
}

func (a *app) recurring(ctx context.Context, today time.Time) ([]recurring, error) {
	type mark struct{ state, at string }
	marks := map[string]mark{}
	mrows, err := a.db.QueryContext(ctx, `SELECT merchant_key,state,marked_at FROM recurring_marks`)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var k string
		var m mark
		if err := mrows.Scan(&k, &m.state, &m.at); err != nil {
			mrows.Close()
			return nil, err
		}
		marks[k] = m
	}
	mrows.Close()
	// Outflows, plus deposits categorized as Income (refunds and other inflows never count).
	rows, err := a.db.QueryContext(ctx, `SELECT COALESCE(mr.root,c.merchant_key),c.merchant_key,COALESCE(mr.display_name,NULLIF(c.merchant,''),c.name),c.name,c.effective_category,c.date,c.amount
		FROM cashflow c LEFT JOIN merchant_roots mr ON mr.key=c.merchant_key
		WHERE (c.amount<0 OR c.effective_category='Income') AND c.amount<>0 AND c.merchant_key!='' ORDER BY c.date,c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		key, raw, name, desc, category string
		income                         bool
		c                              charge
	}
	var all []row
	for rows.Next() {
		var r row
		var d string
		if err := rows.Scan(&r.key, &r.raw, &r.name, &r.desc, &r.category, &d, &r.c.amount); err != nil {
			return nil, err
		}
		if r.c.date, err = time.Parse(time.DateOnly, d); err != nil {
			return nil, err
		}
		r.income = r.c.amount > 0
		r.c.amount = max(r.c.amount, -r.c.amount)
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
	// Income never joins: an import's generic "Paycheck" merchant would chain two employers' payrolls into one.
	byDesc := map[string]map[string]bool{}
	for _, r := range all {
		if d, _ := cleanMerchant(r.desc); !r.income && strings.Contains(d, " ") {
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
	type group struct {
		key    string
		income bool
	}
	groups := map[group][]row{}
	for _, r := range all {
		g := group{find(r.key), r.income}
		groups[g] = append(groups[g], r)
	}
	out := make([]recurring, 0)
	for g, rs := range groups {
		cs := make([]charge, len(rs))
		for i, r := range rs {
			cs[i] = r.c
		}
		last := rs[len(rs)-1] // the latest charge's merchant, name and category win
		kind := "subscription"
		if g.income {
			kind = "income"
		} else if billCategories[last.category] {
			kind = "bill"
		}
		r, ok := detectRecurring(cs, today, kind)
		if !ok {
			continue
		}
		r.Key, r.Name, r.Category = last.key, last.name, last.category
		// ponytail: marks are per merchant, so a merchant with both recurring income and charges shares one mark.
		var m mark
		for _, x := range rs {
			for _, k := range []string{x.key, x.raw} {
				if mk, ok := marks[k]; ok && mk.at >= m.at {
					m, r.MarkKey = mk, k
				}
			}
		}
		r.Mark = m.state
		r.ChargedAfterCancel = m.state == "cancelled" && r.Last > m.at
		out = append(out, r)
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
	mux.HandleFunc("PUT /api/recurring/{key}/mark", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			State string `json:"state"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.State != "cancelled" && input.State != "hidden" {
			jsonResponse(w, 400, map[string]string{"error": "state must be cancelled or hidden"})
			return
		}
		if _, err := a.db.ExecContext(r.Context(), `INSERT INTO recurring_marks(merchant_key,state,marked_at) VALUES($1,$2,$3)
			ON CONFLICT(merchant_key) DO UPDATE SET state=excluded.state,marked_at=excluded.marked_at`, r.PathValue("key"), input.State, time.Now().Format(time.DateOnly)); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"mark": input.State})
	})
	mux.HandleFunc("DELETE /api/recurring/{key}/mark", func(w http.ResponseWriter, r *http.Request) {
		if _, err := a.db.ExecContext(r.Context(), `DELETE FROM recurring_marks WHERE merchant_key=$1`, r.PathValue("key")); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"mark": ""})
	})
}
