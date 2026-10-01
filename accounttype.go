package main

import (
	"database/sql"
	"net/http"
	"regexp"
	"strings"
)

// Account types money-tracker knows. The values match what the UI groups by (web/main.js TYPE_GROUPS).
var accountTypes = map[string]bool{"depository": true, "credit": true, "investment": true, "loan": true, "property": true, "other": true}

// typeRules are tried in order against the account name, then against institution+name; first match wins.
// Order matters: "line of credit" and "hsa investment" must beat the broader "credit" and "hsa" rules. To extend, add a row.
var typeRules = []struct {
	typ string
	re  *regexp.Regexp
}{
	{"loan", typeRE(`mortgage|heloc|auto loan|student loan|loan|line of credit`)},
	{"investment", typeRE(`brokerage|401\s?\(?k\)?|403\s?\(?b\)?|457|ira|roth|hsa investment|individual|trust|pension|investments?`)},
	{"credit", typeRE(`credit card|visa|mastercard|amex|american express|discover|sapphire|freedom|venture|quicksilver|rewards card|card`)},
	{"depository", typeRE(`checking|savings|money market|cd|hsa`)},
}

// typeRE matches alternatives as whole words; \b would not work after a closing paren as in "401(k)".
func typeRE(alternatives string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^a-z0-9])(?:` + alternatives + `)(?:[^a-z0-9]|$)`)
}

// guessAccountType suggests a type from the names (and balance sign as a last resort). confident is false for the fallbacks.
func guessAccountType(institution, name string, balanceCents int64) (typ string, confident bool) {
	name = strings.ToLower(name)
	both := strings.ToLower(institution) + " " + name
	for _, text := range []string{name, both} {
		for _, r := range typeRules {
			if r.re.MatchString(text) {
				return r.typ, true
			}
		}
	}
	if balanceCents < 0 {
		return "credit", false
	}
	return "other", false
}

func (a *app) accountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PATCH /api/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Type *string `json:"type"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.Type == nil || (*input.Type != "" && !accountTypes[*input.Type]) {
			jsonResponse(w, 400, map[string]string{"error": "type must be one of depository, credit, investment, loan, other, or empty to clear"})
			return
		}
		id := r.PathValue("id")
		var institution, name string
		var current sql.NullInt64
		err := a.db.QueryRowContext(r.Context(), `SELECT institution,name,current FROM accounts WHERE id=$1`, id).Scan(&institution, &name, &current)
		if err == sql.ErrNoRows {
			jsonResponse(w, 404, map[string]string{"error": "account not found"})
			return
		}
		if err != nil {
			apiError(w, err)
			return
		}
		// Recompute the guess too, so clearing an override takes effect immediately.
		guess, confident := guessAccountType(institution, name, current.Int64)
		if err := a.db.QueryRowContext(r.Context(), `UPDATE accounts SET user_type=$1,guessed_type=`+guessedTypeSQL("accounts.name", "$2", "$3::boolean")+`,guess_confident=$3::boolean OR `+jevKept("accounts.name", "$3::boolean")+`
			WHERE id=$4 RETURNING guessed_type,guess_confident`, *input.Type, guess, confident, id).Scan(&guess, &confident); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"id": id, "type": firstNonEmpty(*input.Type, guess), "type_source": typeSource(*input.Type), "type_confident": *input.Type != "" || confident})
	})
}

func typeSource(userType string) string {
	if userType != "" {
		return "user"
	}
	return "guess"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
