package main

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"strings"
)

// Merchant model: transactions.merchant_key is derived from the raw merchant/name by cleanMerchant.
// merchants holds one row per key; merging points a key at a target via merged_into (kept flat),
// and the merchant_roots view resolves any key to its target. Reads resolve at query time.

// displayMerchantSQL is the resolved display merchant for a transactions alias t (falls back to the raw field).
const displayMerchantSQL = `COALESCE((SELECT display_name FROM merchant_roots WHERE key=t.merchant_key),t.merchant)`

type merchantAlias struct {
	re      *regexp.Regexp
	display string
}

type prefixRule struct {
	re          *regexp.Regexp
	afterPrefix bool // only strips when an earlier rule already stripped something (a bare 4-digit MMDD)
}

// Table-driven cleaner rules. Input is lowercase.
var (
	merchantPrefixes = []prefixRule{
		{regexp.MustCompile(`^purchase authorized on \d\d/\d\d\s*`), false},
		{regexp.MustCompile(`^(?:pos|debit|card|checkcard|check card|purchase|recurring|ach|visa|aplpay|apl pay|pending)\b(?:\s+(?:debit|credit|purchase|card|payment|withdrawal))*\s*`), false},
		{regexp.MustCompile(`^(?:sq|tst|pp|dd|paypal|sp)\s*\*\s*`), false},
		{regexp.MustCompile(`^\d\d/\d\d\s+`), false},
		{regexp.MustCompile(`^\d{4}\s+`), true},
	}
	merchantAliases = []merchantAlias{
		{regexp.MustCompile(`^(?:amzn mktp|amazon|amzn)\b`), "Amazon"},
		{regexp.MustCompile(`^(?:wal-?mart|wm supercenter)\b`), "Walmart"},
		{regexp.MustCompile(`^uber\s*\*?\s*eats\b`), "Uber Eats"},
		{regexp.MustCompile(`^uber\b`), "Uber"},
		{regexp.MustCompile(`^apple\.com`), "Apple"},
		{regexp.MustCompile(`^cvs\b`), "CVS"},
	}
	merchantJunk   = regexp.MustCompile(`[^a-z0-9#&'\-. ]+`)
	merchantDomain = regexp.MustCompile(`\.(?:com|net|org)\b`)
	usStates       = " al ak az ar ca co ct de fl ga hi id il in ia ks ky la me md ma mi mn ms mo mt ne nv nh nj nm ny nc nd oh ok or pa ri sc sd tn tx ut vt va wa wv wi wy dc "
)

func isState(word string) bool { return len(word) == 2 && strings.Contains(usStates, " "+word+" ") }

// chargeState is the US state a bank description ends with ("SAFEWAY #1234 DENVER CO" -> "CO"), else "".
func chargeState(raw string) string {
	fields := strings.Fields(merchantJunk.ReplaceAllString(strings.ToLower(raw), " "))
	if n := len(fields); n >= 2 && isState(fields[n-1]) {
		return strings.ToUpper(fields[n-1])
	}
	return ""
}

// cleanMerchant turns a raw bank description into a merchant key and a readable display name.
// ponytail: heuristic; a trailing "city state" is only dropped as one city word, so multi-word cities
// without a store number can leave a stray word (fix with a rename or merge).
func cleanMerchant(raw string) (key, display string) {
	s := strings.ToLower(strings.TrimSpace(raw))
	for stripped, changed := false, true; changed; {
		changed = false
		for _, p := range merchantPrefixes {
			if p.afterPrefix && !stripped {
				continue
			}
			if loc := p.re.FindStringIndex(s); loc != nil && loc[1] > 0 {
				s, changed, stripped = s[loc[1]:], true, true
			}
		}
	}
	for _, a := range merchantAliases {
		if a.re.MatchString(s) {
			return strings.ToLower(a.display), a.display
		}
	}
	fields := strings.Fields(merchantJunk.ReplaceAllString(merchantDomain.ReplaceAllString(s, ""), " "))
	// Everything after the first store number, reference number, card mask or "#" is location noise.
	for i, f := range fields {
		if i > 0 && (strings.HasPrefix(f, "#") || strings.HasPrefix(f, "xxx") || strings.ContainsAny(f, "0123456789")) {
			fields = fields[:i]
			break
		}
	}
	if n := len(fields); n >= 2 && isState(fields[n-1]) {
		fields = fields[:n-1]
		if n-1 >= 3 {
			fields = fields[:n-2]
		}
	}
	for i, f := range fields {
		fields[i] = strings.Trim(f, ".-")
	}
	fields = strings.Fields(strings.Join(fields, " "))
	if len(fields) == 0 {
		if s = strings.TrimSpace(strings.ToLower(raw)); s == "" {
			s = "unknown"
		}
		return s, s
	}
	key = strings.Join(fields, " ")
	for i, f := range fields {
		fields[i] = strings.ToUpper(f[:1]) + f[1:]
	}
	return key, strings.Join(fields, " ")
}

// assignMerchantKeys fills merchant_key (and the merchants row) for transactions that lack one.
// Called after every sync upsert and at startup, so it also backfills rows that pre-date the migration.
func assignMerchantKeys(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) error {
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(NULLIF(merchant,''),name) FROM transactions WHERE merchant_key=''`)
	if err != nil {
		return err
	}
	type todo struct{ id, key, display string }
	var todos []todo
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		key, display := cleanMerchant(raw)
		todos = append(todos, todo{id, key, display})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, t := range todos {
		if _, err := db.ExecContext(ctx, `INSERT INTO merchants(key,display_name) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, t.key, t.display); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `UPDATE transactions SET merchant_key=$1 WHERE id=$2`, t.key, t.id); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) merchantRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/merchants", func(w http.ResponseWriter, r *http.Request) {
		// transactions counts a target's merged keys too.
		rows, err := a.db.QueryContext(r.Context(), `SELECT m.key,m.display_name,m.merged_into,m.category,COALESCE(c.n,0)
			FROM merchants m LEFT JOIN (SELECT mr.root,COUNT(*) n FROM transactions t JOIN merchant_roots mr ON mr.key=t.merchant_key GROUP BY mr.root) c ON c.root=m.key
			ORDER BY m.display_name,m.key`)
		queryRows(w, rows, err, func(rows *sql.Rows) (any, error) {
			var key, display, mergedInto, category string
			var count int
			err := rows.Scan(&key, &display, &mergedInto, &category, &count)
			return map[string]any{"key": key, "display_name": display, "merged_into": mergedInto, "category": category, "transactions": count}, err
		})
	})
	mux.HandleFunc("PATCH /api/merchants/{key}", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			DisplayName string `json:"display_name"`
		}
		if !decode(w, r, &input) {
			return
		}
		name, ok := cleanField(w, "display_name", input.DisplayName)
		if !ok {
			return
		}
		key := r.PathValue("key")
		var mergedInto string
		if err := a.db.QueryRowContext(r.Context(), `SELECT merged_into FROM merchants WHERE key=$1`, key).Scan(&mergedInto); err == sql.ErrNoRows {
			jsonResponse(w, 404, map[string]string{"error": "merchant not found"})
			return
		} else if err != nil {
			apiError(w, err)
			return
		}
		if mergedInto != "" {
			jsonResponse(w, 409, map[string]string{"error": "merchant is merged into " + mergedInto + "; rename that one"})
			return
		}
		if _, err := a.db.ExecContext(r.Context(), `UPDATE merchants SET display_name=$1 WHERE key=$2`, name, key); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"key": key, "display_name": name})
	})
	mux.HandleFunc("POST /api/merchants/merge", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Target  string   `json:"target"`
			Sources []string `json:"sources"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.Target == "" || len(input.Sources) == 0 {
			jsonResponse(w, 400, map[string]string{"error": "target and at least one source are required"})
			return
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			apiError(w, err)
			return
		}
		defer tx.Rollback()
		var target string // a merged target resolves to its own target, keeping merges flat
		if err := tx.QueryRowContext(r.Context(), `SELECT root FROM merchant_roots WHERE key=$1`, input.Target).Scan(&target); err == sql.ErrNoRows {
			jsonResponse(w, 404, map[string]string{"error": "target merchant not found"})
			return
		} else if err != nil {
			apiError(w, err)
			return
		}
		for _, source := range input.Sources {
			if source == target || source == input.Target {
				jsonResponse(w, 400, map[string]string{"error": "cannot merge a merchant into itself"})
				return
			}
			var n int
			if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM merchants WHERE key=$1`, source).Scan(&n); err != nil {
				apiError(w, err)
				return
			}
			if n == 0 {
				jsonResponse(w, 404, map[string]string{"error": "source merchant not found: " + source})
				return
			}
			// Repoint anything already merged into source, then source itself.
			if _, err := tx.ExecContext(r.Context(), `UPDATE merchants SET merged_into=$1 WHERE merged_into=$2 OR key=$2`, target, source); err != nil {
				apiError(w, err)
				return
			}
		}
		if err := applyRules(r.Context(), tx); err != nil { // a different merchant category may now apply
			apiError(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"target": target, "sources": input.Sources})
	})
	a.suggestionRoutes(mux)
}
