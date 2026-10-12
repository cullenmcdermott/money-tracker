package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Invest a deposit: split an amount across an investment account's funds so they move toward a target balance,
// filling whatever is furthest below its target first and never selling.
//
//	GET /api/invest/{account}?amount=<cents>  the funds, their targets and the split
//	POST /api/invest/{account}/preview?amount= the same for an unsaved investDoc
//	PUT /api/invest/{account}                 save the account's investDoc
//
// The target comes from the owner's purchases by default: each fund's share of the buys over the last Months.
// SimpleFIN sends a buy as a negative row named after the fund; a reinvested dividend comes as the same name with
// a matching positive row the same day, so those pairs are left out.

type investDoc struct {
	Mode    string         `json:"mode"`              // purchases, percent or classes
	Months  int            `json:"months"`            // purchases lookback
	Percent map[string]int `json:"percent,omitempty"` // mode percent: whole percent per symbol
	Classes [3]int         `json:"classes"`           // mode classes: US stock, international stock, bonds, whole percent
	Ignore  []string       `json:"ignore,omitempty"`  // symbols held but not bought any more
}

var defaultInvestDoc = investDoc{Mode: "purchases", Months: 12, Classes: [3]int{60, 30, 10}}

type investFund struct {
	Symbol      string     `json:"symbol"`
	Description string     `json:"description"`
	Value       int64      `json:"value"`
	Bought      int64      `json:"bought"` // in the lookback
	Class       *fundClass `json:"class,omitempty"`
	Ignored     bool       `json:"ignored,omitempty"`
	Target      float64    `json:"target"` // share of the account's target funds, 0..1
	Buy         int64      `json:"buy"`
}

func (a *app) investRoutes(mux *http.ServeMux) {
	// GET shows the saved targets; POST .../preview tries an unsaved investDoc and saves nothing.
	serve := func(w http.ResponseWriter, r *http.Request, try *investDoc) {
		amount, _ := strconv.ParseInt(r.URL.Query().Get("amount"), 10, 64)
		out, err := a.invest(r.Context(), r.PathValue("account"), max(amount, 0), try)
		if errors.Is(err, sql.ErrNoRows) {
			jsonResponse(w, 404, map[string]string{"error": "No such investment account."})
			return
		}
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, out)
	}
	mux.HandleFunc("GET /api/invest/{account}", func(w http.ResponseWriter, r *http.Request) { serve(w, r, nil) })
	mux.HandleFunc("POST /api/invest/{account}/preview", func(w http.ResponseWriter, r *http.Request) {
		var d investDoc
		if !decode(w, r, &d) {
			return
		}
		if msg := d.check(); msg != "" {
			jsonResponse(w, 400, map[string]string{"error": msg})
			return
		}
		serve(w, r, &d)
	})
	mux.HandleFunc("PUT /api/invest/{account}", func(w http.ResponseWriter, r *http.Request) {
		var d investDoc
		if !decode(w, r, &d) {
			return
		}
		if msg := d.check(); msg != "" {
			jsonResponse(w, 400, map[string]string{"error": msg})
			return
		}
		b, _ := json.Marshal(d)
		res, err := a.db.ExecContext(r.Context(), `INSERT INTO invest_settings(account_id,doc) SELECT id,$2 FROM accounts WHERE id=$1 AND type='investment'
			ON CONFLICT(account_id) DO UPDATE SET doc=$2`, r.PathValue("account"), b)
		if err != nil {
			apiError(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			jsonResponse(w, 404, map[string]string{"error": "No such investment account."})
			return
		}
		jsonResponse(w, 200, map[string]bool{"saved": true})
	})
}

func (d investDoc) check() string {
	sum := func(v []int) int {
		s := 0
		for _, x := range v {
			if x < 0 || x > 100 {
				return -1
			}
			s += x
		}
		return s
	}
	switch {
	case d.Mode != "purchases" && d.Mode != "percent" && d.Mode != "classes":
		return "Pick how to set the targets."
	case d.Months < 1 || d.Months > 120:
		return "Look back between 1 and 120 months."
	case d.Mode == "percent" && sum(slices.Collect(maps.Values(d.Percent))) != 100:
		return "Make the fund percentages whole numbers that total 100."
	case d.Mode == "classes" && sum(d.Classes[:]) != 100:
		return "Make the class percentages whole numbers that total 100."
	}
	return ""
}

func (a *app) invest(ctx context.Context, account string, amount int64, try *investDoc) (map[string]any, error) {
	if err := a.db.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id=$1 AND type='investment'`, account).Scan(new(int)); err != nil {
		return nil, err
	}
	doc := defaultInvestDoc
	var raw []byte
	switch err := a.db.QueryRowContext(ctx, `SELECT doc FROM invest_settings WHERE account_id=$1`, account).Scan(&raw); {
	case try != nil:
		doc = *try
	case err == nil:
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	list := []investmentAccount{{ID: account}}
	if _, err := a.addPositions(ctx, list); err != nil {
		return nil, err
	}
	funds := []investFund{} // [] rather than null: an account may hold no funds
	for _, p := range list[0].Positions {
		if p.Class == moneyMarket { // cash waiting to be invested, not a fund to buy
			continue
		}
		funds = append(funds, investFund{Symbol: p.Symbol, Description: p.Description, Value: p.Value, Class: p.Class,
			Ignored: slices.Contains(doc.Ignore, p.Symbol)})
	}

	rows, err := a.db.QueryContext(ctx, `SELECT name, -amount FROM investment_activity ia WHERE account_id=$1 AND kind='internal' AND amount<0 AND date>=$2
		AND NOT EXISTS (SELECT 1 FROM transactions o WHERE o.account_id=ia.account_id AND o.date=ia.date AND o.name=ia.name AND o.amount=-ia.amount)`,
		account, time.Now().AddDate(0, -doc.Months, 0).Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	unmatched := map[string]int64{}
	for rows.Next() {
		var n string
		var amt int64
		if err := rows.Scan(&n, &amt); err != nil {
			return nil, err
		}
		if i := matchFund(n, funds); i >= 0 {
			funds[i].Bought += amt
		} else if !noFund.MatchString(n) {
			unmatched[n] += amt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	note := setTargets(funds, doc)
	split(funds, amount)
	return map[string]any{"account": account, "doc": doc, "amount": amount, "funds": funds, "unmatched": unmatched, "note": note}, nil
}

// Rows that name no fund: money market sweeps, and the bare actions in rows imported from Monarch.
var noFund = regexp.MustCompile(`(?i)money market|^(buy|sell|reinvest|sweep)`)

var fundWord = regexp.MustCompile(`[a-z0-9]+`)

// matchFund finds the fund a buy row names, by word overlap (Jaccard) between the row and each holding's
// description, ignoring words every fund name shares. It returns an index into funds, or -1.
// ponytail: word overlap, not a ticker lookup; a buy that names no held fund well enough stays unmatched.
func matchFund(name string, funds []investFund) int {
	words := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, w := range fundWord.FindAllString(strings.ToLower(s), -1) {
			switch w {
			case "vanguard", "fund", "index", "idx", "admiral", "admrl", "investor", "inv", "cl", "class", "shares", "the", "inc", "trust", "etf":
			default:
				out[w] = true
			}
		}
		return out
	}
	row, best, bestScore := words(name), -1, 0.5
	for i, f := range funds {
		if strings.EqualFold(strings.TrimSpace(name), f.Symbol) {
			return i
		}
		d := words(f.Description)
		both := 0
		for w := range row {
			if d[w] {
				both++
			}
		}
		if s := float64(both) / float64(len(row)+len(d)-both); both > 0 && s >= bestScore {
			best, bestScore = i, s
		}
	}
	return best
}

// setTargets fills each fund's Target (shares summing to 1 over the funds bought toward) from the doc's mode,
// and returns a note when it had to fall back or leave something out.
func setTargets(funds []investFund, doc investDoc) string {
	w := make([]float64, len(funds))
	by := func(get func(investFund) int64, idx []int) {
		var t int64
		for _, i := range idx {
			t += get(funds[i])
		}
		for _, i := range idx {
			if t > 0 {
				w[i] = float64(get(funds[i])) / float64(t)
			} else {
				w[i] = 1 / float64(len(idx))
			}
		}
	}
	var live []int
	for i, f := range funds {
		if !f.Ignored {
			live = append(live, i)
		}
	}
	bought := func(f investFund) int64 { return f.Bought }
	value := func(f investFund) int64 { return f.Value }
	anyBought := slices.ContainsFunc(live, func(i int) bool { return funds[i].Bought > 0 })
	note := ""
	switch doc.Mode {
	case "percent": // a fund's own percentage stands, ignored or not
		for i, f := range funds {
			w[i] = float64(doc.Percent[f.Symbol]) / 100
		}
	case "classes":
		// Each fund counts toward its largest class; within a class, funds keep the mix you buy them in.
		var missing []string
		for c, label := range []string{"US stock", "international stock", "bonds"} {
			var in []int
			for _, i := range live {
				if f := funds[i]; f.Class != nil && slices.Index(f.Class.Split[:], slices.Max(f.Class.Split[:])) == c {
					in = append(in, i)
				}
			}
			if len(in) == 0 {
				if doc.Classes[c] > 0 {
					missing = append(missing, label)
				}
				continue
			}
			if slices.ContainsFunc(in, func(i int) bool { return funds[i].Bought > 0 }) {
				by(bought, in)
			} else {
				by(value, in)
			}
			for _, i := range in {
				w[i] *= float64(doc.Classes[c]) / 100
			}
		}
		var unclassified []string
		for _, i := range live {
			if funds[i].Class == nil {
				unclassified = append(unclassified, funds[i].Symbol)
			}
		}
		if len(unclassified) > 0 {
			note = "Classify " + strings.Join(unclassified, ", ") + " under Holdings on the Investments page to count them."
		}
		if len(missing) > 0 {
			note = strings.TrimSpace(note + " No fund here holds mostly " + strings.Join(missing, " or ") + ", so that part of the target is left out.")
		}
	default:
		if anyBought {
			by(bought, live)
		} else {
			by(value, live)
			note = "No purchases found in the lookback, so the targets follow what each fund is worth today."
		}
	}
	var t float64
	for _, x := range w {
		t += x
	}
	for i := range funds {
		if t > 0 {
			funds[i].Target = w[i] / t
		}
	}
	return note
}

// split sets each fund's Buy so the funds move as close to their targets as the amount allows without selling:
// it raises a common level L until sum(max(0, target*L - value)) uses the whole amount (water-filling), so the
// most underweight funds fill first. Rounding cents go to or come from the largest buy.
func split(funds []investFund, amount int64) {
	if amount <= 0 {
		return
	}
	var total float64
	minT := 1.0
	for _, f := range funds {
		if f.Target > 0 {
			total += float64(f.Value)
			minT = min(minT, f.Target)
		}
	}
	if !slices.ContainsFunc(funds, func(f investFund) bool { return f.Target > 0 }) {
		return
	}
	need := func(L float64) (s float64) {
		for _, f := range funds {
			if f.Target > 0 {
				s += max(0, f.Target*L-float64(f.Value))
			}
		}
		return s
	}
	lo, hi := 0.0, (total+float64(amount))/minT
	for range 100 {
		if mid := (lo + hi) / 2; need(mid) < float64(amount) {
			lo = mid
		} else {
			hi = mid
		}
	}
	left, big := amount, -1
	for i := range funds {
		if funds[i].Target > 0 {
			funds[i].Buy = int64(math.Round(max(0, funds[i].Target*lo-float64(funds[i].Value))))
			left -= funds[i].Buy
			if big < 0 || funds[i].Buy > funds[big].Buy {
				big = i
			}
		}
	}
	funds[big].Buy += left
}
