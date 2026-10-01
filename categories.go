package main

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

var defaultCategories = []string{"Income", "Housing", "Groceries", "Dining", "Coffee Shops", "Transportation", "Utilities", "Shopping", "Health", "Pets", "Haircuts",
	"Entertainment", "Video Games", "Travel", "Subscriptions", "Insurance", "Donations", "Other", "Transfer"}

// cleanField trims s and checks it is 1-60 characters.
func cleanField(w http.ResponseWriter, name, s string) (string, bool) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > 60 {
		jsonResponse(w, 400, map[string]string{"error": name + " must be 1-60 characters"})
		return "", false
	}
	return s, true
}

// categoryNames is the defaults followed by every other category in use.
func (a *app) categoryNames(ctx context.Context) ([]string, error) {
	// Transfers are left out of the cashflow view, so their categories are not offered as choices.
	rows, err := a.db.QueryContext(ctx, `SELECT c FROM (SELECT effective_category c FROM cashflow UNION SELECT user_category FROM transactions UNION SELECT category FROM rules) u WHERE c != '' ORDER BY c`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := append([]string{}, defaultCategories...)
	seen := make(map[string]bool)
	for _, c := range result {
		seen[c] = true
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		if !seen[c] {
			result = append(result, c)
		}
	}
	return result, rows.Err()
}

func (a *app) categoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/categories", func(w http.ResponseWriter, r *http.Request) {
		// Transfers are left out of the cashflow view, so their categories are not offered as choices.
		result, err := a.categoryNames(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	mux.HandleFunc("PATCH /api/transactions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Category *string `json:"category"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.Category == nil {
			jsonResponse(w, 400, map[string]string{"error": "category is required (empty string clears the manual category)"})
			return
		}
		category := strings.TrimSpace(*input.Category)
		if category != "" {
			var ok bool
			if category, ok = cleanField(w, "category", category); !ok {
				return
			}
		}
		id := r.PathValue("id")
		result, err := a.db.ExecContext(r.Context(), `UPDATE transactions SET user_category=$1 WHERE id=$2`, category, id)
		if err != nil {
			apiError(w, err)
			return
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			jsonResponse(w, 404, map[string]string{"error": "transaction not found"})
			return
		}
		// Clearing hands the transaction back to the rules, so recompute.
		if err := applyRules(r.Context(), a.db); err != nil {
			apiError(w, err)
			return
		}
		var effective string
		if err := a.db.QueryRowContext(r.Context(), `SELECT effective_category FROM transactions WHERE id=$1`, id).Scan(&effective); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"id": id, "category": effective, "manual": category != ""})
	})
	mux.HandleFunc("GET /api/rules", func(w http.ResponseWriter, r *http.Request) {
		rows, err := a.db.QueryContext(r.Context(), `SELECT id,pattern,category FROM rules ORDER BY id`)
		queryRows(w, rows, err, func(rows *sql.Rows) (any, error) {
			var id int64
			var pattern, category string
			err := rows.Scan(&id, &pattern, &category)
			return map[string]any{"id": id, "pattern": pattern, "category": category}, err
		})
	})
	mux.HandleFunc("POST /api/rules", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Pattern  string `json:"pattern"`
			Category string `json:"category"`
		}
		if !decode(w, r, &input) {
			return
		}
		pattern, ok := cleanField(w, "pattern", input.Pattern)
		if !ok {
			return
		}
		category, ok := cleanField(w, "category", input.Category)
		if !ok {
			return
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			apiError(w, err)
			return
		}
		defer tx.Rollback()
		var id int64 // the unique index is on lower(pattern), so a case-variant duplicate conflicts too
		err = tx.QueryRowContext(r.Context(), `INSERT INTO rules(pattern,category) VALUES($1,$2) ON CONFLICT DO NOTHING RETURNING id`, pattern, category).Scan(&id)
		if err == sql.ErrNoRows {
			jsonResponse(w, 409, map[string]string{"error": "a rule for that pattern already exists"})
			return
		} else if err != nil {
			apiError(w, err)
			return
		}
		if err := applyRules(r.Context(), tx); err != nil {
			apiError(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 201, map[string]any{"id": id, "pattern": pattern, "category": category})
	})
	mux.HandleFunc("DELETE /api/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		ruleID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonResponse(w, 404, map[string]string{"error": "rule not found"})
			return
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			apiError(w, err)
			return
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(r.Context(), `DELETE FROM rules WHERE id=$1`, ruleID)
		if err != nil {
			apiError(w, err)
			return
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			jsonResponse(w, 404, map[string]string{"error": "rule not found"})
			return
		}
		if err := applyRules(r.Context(), tx); err != nil {
			apiError(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"status": "ok"})
	})
}
