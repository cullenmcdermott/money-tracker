package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
)

// Suggestions are per merchant and never applied automatically: GET /api/suggestions lists them,
// POST /api/suggestions/accept applies one.
//
// Precedence with existing categories (manual > rule > remembered merchant category) is kept:
//   - accept without remember writes user_category on the merchant's currently uncategorized cashflow rows
//     (a manual choice, so sync and rules never override it);
//   - accept with remember stores merchants.category on the target merchant; applyRules folds it into
//     rule_category for rows with no manual category and no matching rule, so it ranks below both and
//     and applies to future synced rows and merged keys.
//
// Neither touches transfer_id, so transfer detection and the cashflow view are unaffected.

// categoryDescriptions are the short category definitions handed to sources (a remote model needs them as option descriptions).
var categoryDescriptions = map[string]string{
	"Income":         "Paychecks, interest, refunds, money coming in",
	"Housing":        "Rent, mortgage, property tax, home maintenance",
	"Groceries":      "Supermarkets and food shopping for the home",
	"Dining":         "Restaurants, bars, takeout, delivery",
	"Coffee Shops":   "Coffee shops and cafes",
	"Transportation": "Gas, transit, rideshare, parking, car costs",
	"Utilities":      "Electricity, gas, water, internet, phone",
	"Shopping":       "General retail and online purchases",
	"Health":         "Doctors, pharmacy, dental, therapy, fitness and gyms",
	"Pets":           "Vets, pet food and supplies, pet pharmacy, pet insurance",
	"Haircuts":       "Barbers and hair salons",
	"Entertainment":  "Movies, concerts, theater, events, museums",
	"Video Games":    "Video games and in-game purchases (Steam, consoles)",
	"Travel":         "Flights, hotels, rentals, vacation spending",
	"Subscriptions":  "Recurring streaming, software, and membership fees",
	"Insurance":      "Auto, home, health, and life insurance premiums",
	"Donations":      "Charities, nonprofits, and political donations",
	"Transfer":       "Moving your own money to savings, a brokerage, a credit card payment or a Venmo top-up; not spending",
	"Other":          "Anything that fits none of the above",
}

// MerchantSample is what a source sees for one merchant: no transaction ids, dates or account details.
type MerchantSample struct {
	Key           string   // merchant key (target merchant after merges)
	Display       string   // resolved display name
	RawNames      []string // up to 3 distinct raw bank descriptions
	Count         int      // uncategorized transactions
	Total         int64    // their summed amount in cents (negative = spending)
	TypicalAmount int64    // Total / Count, cents
	Charges       int      // all its cashflow rows, categorized or not
	TypicalGap    int      // median days between those charges; 0 when fewer than 3
}

type CategoryChoice struct{ Name, Description string }

type Suggestion struct {
	Category      string
	Confidence    float64            // 0..1
	Probabilities map[string]float64 // optional per-category probabilities (remote models)
	Source        string
	Reason        string
}

// SuggestionSource proposes categories for a batch of merchants. Merchants it has no opinion on are
// simply absent from the result. Batching keeps a remote model to one call per request.
type SuggestionSource interface {
	Name() string
	Suggest(ctx context.Context, samples []MerchantSample, categories []CategoryChoice) (map[string]Suggestion, error)
}

// suggestionSources is the seam: sources are consulted in order, and each merchant takes the first
// source that answers, so Jev (only when JEV_API_KEY is set) sees just what the local sources could not.
// A remote source sends only MerchantSample data and returns an error on failure (the caller logs it
// and falls through to the next source).
func (a *app) suggestionSources() []SuggestionSource {
	sources := []SuggestionSource{historySource{a.db}, keywordSource{}}
	if a.jev != nil {
		sources = append(sources, jevSource{a.jev, a.db})
	}
	return sources
}

// categoryChoices describes each category for sources; the user's own categories describe themselves by name.
func categoryChoices(names []string) []CategoryChoice {
	out := make([]CategoryChoice, 0, len(names))
	for _, c := range names {
		d := categoryDescriptions[c]
		if d == "" {
			d = c
		}
		out = append(out, CategoryChoice{c, d})
	}
	return out
}

// historySource suggests the category the user (or a rule) most often gave the merchant's other transactions.
type historySource struct{ db *sql.DB }

func (historySource) Name() string { return "history" }

func (h historySource) Suggest(ctx context.Context, samples []MerchantSample, _ []CategoryChoice) (map[string]Suggestion, error) {
	bySample := make(map[string]MerchantSample, len(samples))
	roots := make([]string, 0, len(samples))
	for _, s := range samples {
		bySample[s.Key] = s
		roots = append(roots, s.Key)
	}
	// Every merchant's categories in one query, each merchant's most common first.
	rows, err := h.db.QueryContext(ctx, `SELECT root, c, COUNT(*) FROM (
			SELECT mr.root, COALESCE(NULLIF(t.user_category,''),NULLIF(t.rule_category,'')) c
			FROM transactions t JOIN merchant_roots mr ON mr.key=t.merchant_key
			WHERE t.transfer_id IS NULL AND mr.root=ANY($1)) u
		WHERE c IS NOT NULL GROUP BY 1,2 ORDER BY 1, 3 DESC, 2`, roots)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type tally struct {
		best         string
		bestN, total int
	}
	tallies := map[string]*tally{}
	for rows.Next() {
		var root, c string
		var n int
		if err := rows.Scan(&root, &c, &n); err != nil {
			return nil, err
		}
		if _, ok := bySample[root]; !ok {
			continue
		}
		if tallies[root] == nil {
			tallies[root] = &tally{best: c, bestN: n}
		}
		tallies[root].total += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]Suggestion)
	for root, tl := range tallies {
		if tl.bestN*2 <= tl.total { // need a strict majority
			continue
		}
		// Confidence = consistency, discounted for small samples: 4 of 4 -> 0.80, 1 of 1 -> 0.50.
		conf := float64(tl.bestN) / float64(tl.total) * float64(tl.total) / float64(tl.total+1)
		out[root] = Suggestion{Category: tl.best, Confidence: math.Round(conf*100) / 100, Source: "history",
			Reason: fmt.Sprintf("You've put %d of %d %s transactions in %s", tl.bestN, tl.total, bySample[root].Display, tl.best)}
	}
	return out, nil
}

// keywordSource is a small built-in map from merchant-name keywords to categories.
// ponytail: substring match on lowercase names, fixed 0.6 confidence; extend the list or use a model when it misses too much.
type keywordSource struct{}

var keywordCategories = []struct {
	category string
	words    []string
}{
	{"Groceries", []string{"soopers", "safeway", "trader joe", "whole foods", "kroger", "costco", "aldi", "sprouts", "publix", "instacart"}},
	{"Transportation", []string{"shell", "conoco", "exxon", "chevron", "bp ", "sinclair", "uber", "lyft", "parking"}},
	{"Subscriptions", []string{"netflix", "spotify", "hulu", "disney", "youtube", "apple music", "icloud", "patreon"}},
	{"Utilities", []string{"xcel", "comcast", "xfinity", "verizon", "t-mobile", "at&t", "centurylink"}},
	{"Dining", []string{"starbucks", "chipotle", "mcdonald", "blue bottle", "doordash", "uber eats", "grubhub", "pizza", "coffee"}},
	{"Health", []string{"cvs", "walgreens", "pharmacy", "dental"}},
	{"Insurance", []string{"geico", "state farm", "progressive", "allstate"}},
}

func (keywordSource) Name() string { return "keyword" }

func (keywordSource) Suggest(_ context.Context, samples []MerchantSample, _ []CategoryChoice) (map[string]Suggestion, error) {
	out := make(map[string]Suggestion)
	for _, s := range samples {
		text := strings.ToLower(s.Display + " " + strings.Join(s.RawNames, " ") + " ")
		for _, k := range keywordCategories {
			for _, w := range k.words {
				if strings.Contains(text, w) {
					out[s.Key] = Suggestion{Category: k.category, Confidence: 0.6, Source: "keyword",
						Reason: fmt.Sprintf("%s matches the keyword %q, which usually means %s", s.Display, strings.TrimSpace(w), k.category)}
					break
				}
			}
			if _, ok := out[s.Key]; ok {
				break
			}
		}
	}
	return out, nil
}

// merchantGroup selects every merchant_key that resolves to the target merchant bound to $1.
const merchantGroup = `merchant_key IN (SELECT key FROM merchant_roots WHERE root=$1)`

// uncategorizedSamples returns one sample per target merchant that has uncategorized cashflow rows.
func (a *app) uncategorizedSamples(ctx context.Context) ([]MerchantSample, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT mr.root, mr.display_name, COUNT(*), SUM(c.amount)
		FROM cashflow c JOIN merchant_roots mr ON mr.key=c.merchant_key
		WHERE c.effective_category='' GROUP BY mr.root, mr.display_name ORDER BY COUNT(*) DESC, mr.root`)
	if err != nil {
		return nil, err
	}
	var samples []MerchantSample
	for rows.Next() {
		var s MerchantSample
		if err := rows.Scan(&s.Key, &s.Display, &s.Count, &s.Total); err != nil {
			rows.Close()
			return nil, err
		}
		s.TypicalAmount = s.Total / int64(s.Count)
		samples = append(samples, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Charges, the typical gap between them, and up to 3 raw bank names, for every merchant in one pass each.
	byRoot := map[string]*MerchantSample{}
	roots := make([]string, 0, len(samples))
	for i := range samples {
		byRoot[samples[i].Key] = &samples[i]
		roots = append(roots, samples[i].Key)
	}
	rows, err = a.db.QueryContext(ctx, `SELECT root, COUNT(*), COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY gap), 0) FROM (
			SELECT mr.root, c.date::date - lag(c.date::date) OVER (PARTITION BY mr.root ORDER BY c.date) gap
			FROM cashflow c JOIN merchant_roots mr ON mr.key=c.merchant_key WHERE mr.root=ANY($1)) g GROUP BY root`, roots)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var root string
		var charges int
		var gap float64
		if err := rows.Scan(&root, &charges, &gap); err != nil {
			rows.Close()
			return nil, err
		}
		if s := byRoot[root]; s != nil {
			s.Charges = charges
			if charges >= 3 {
				s.TypicalGap = int(math.Round(gap))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	rows, err = a.db.QueryContext(ctx, `SELECT DISTINCT mr.root, COALESCE(NULLIF(c.merchant,''),c.name)
		FROM cashflow c JOIN merchant_roots mr ON mr.key=c.merchant_key WHERE c.effective_category='' ORDER BY 1,2`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var root, name string
		if err := rows.Scan(&root, &name); err != nil {
			rows.Close()
			return nil, err
		}
		if s := byRoot[root]; s != nil && len(s.RawNames) < 3 {
			s.RawNames = append(s.RawNames, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return samples, nil
}

// suggest consults sources in order; each merchant takes the first answer. A failing source is logged and skipped.
func (a *app) suggest(ctx context.Context, samples []MerchantSample) map[string]Suggestion {
	result := make(map[string]Suggestion)
	names, err := a.categoryNames(ctx)
	if err != nil {
		log.Printf("suggestions: categories: %v", err)
		names = defaultCategories
	}
	choices := categoryChoices(names)
	remaining := samples
	for _, src := range a.suggestionSources() {
		if len(remaining) == 0 {
			break
		}
		got, err := src.Suggest(ctx, remaining, choices)
		if err != nil {
			log.Printf("suggestion source %s: %v", src.Name(), err)
			continue
		}
		var next []MerchantSample
		for _, s := range remaining {
			if g, ok := got[s.Key]; ok && g.Category != "" {
				if g.Source == "" {
					g.Source = src.Name()
				}
				result[s.Key] = g
			} else {
				next = append(next, s)
			}
		}
		remaining = next
	}
	return result
}

func (a *app) suggestionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/suggestions", func(w http.ResponseWriter, r *http.Request) {
		samples, err := a.uncategorizedSamples(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		suggestions := a.suggest(r.Context(), samples)
		result := make([]map[string]any, 0)
		for _, s := range samples { // already ordered by uncategorized count
			g, ok := suggestions[s.Key]
			if !ok {
				continue
			}
			item := map[string]any{"merchant": s.Key, "display_name": s.Display, "uncategorized_count": s.Count, "uncategorized_total": s.Total,
				"category": g.Category, "source": g.Source, "confidence": g.Confidence, "reason": g.Reason}
			if len(g.Probabilities) > 0 {
				item["probabilities"] = g.Probabilities
			}
			result = append(result, item)
		}
		jsonResponse(w, 200, result)
	})
	// Forget Jev's saved answers so the next inbox load asks it again (e.g. after adding categories).
	mux.HandleFunc("DELETE /api/suggestions/jev", func(w http.ResponseWriter, r *http.Request) {
		res, err := a.db.ExecContext(r.Context(), `DELETE FROM jev_suggestions`)
		if err != nil {
			apiError(w, err)
			return
		}
		n, _ := res.RowsAffected()
		jsonResponse(w, 200, map[string]int64{"cleared": n})
	})
	mux.HandleFunc("POST /api/suggestions/accept", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Merchant string `json:"merchant"`
			Category string `json:"category"`
			Remember bool   `json:"remember"`
		}
		if !decode(w, r, &input) {
			return
		}
		// remember with an empty category clears the merchant's remembered category (the UI's Undo).
		category := ""
		if !input.Remember || strings.TrimSpace(input.Category) != "" {
			var ok bool
			if category, ok = cleanField(w, "category", input.Category); !ok {
				return
			}
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			apiError(w, err)
			return
		}
		defer tx.Rollback()
		var root string
		if err := tx.QueryRowContext(r.Context(), `SELECT root FROM merchant_roots WHERE key=$1`, input.Merchant).Scan(&root); err == sql.ErrNoRows {
			jsonResponse(w, 404, map[string]string{"error": "merchant not found"})
			return
		} else if err != nil {
			apiError(w, err)
			return
		}
		var applied int
		if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM cashflow WHERE effective_category='' AND `+merchantGroup, root).Scan(&applied); err != nil {
			apiError(w, err)
			return
		}
		if input.Remember {
			_, err = tx.ExecContext(r.Context(), `UPDATE merchants SET category=$2 WHERE key=$1`, root, category)
		} else {
			_, err = tx.ExecContext(r.Context(), `UPDATE transactions SET user_category=$2 WHERE id IN
				(SELECT id FROM cashflow WHERE effective_category='' AND `+merchantGroup+`)`, root, category)
		}
		if err == nil {
			err = applyRules(r.Context(), tx) // folds the merchant category into rule_category
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"merchant": root, "category": category, "remember": input.Remember, "applied": applied})
	})
}
