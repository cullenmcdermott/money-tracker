package main

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// GET /api/investments?period=3m|ytd|1y|all: per investment account, where the change over the period came from.
//
//	added    = contributions (investment_activity: transfers, payroll, conversions, and share sales, which is
//	           when $0 vest and ESPP deposits get their value);
//	dividends = income rows;
//	market   = end - start - added - dividends.
//
// Balances come from the balances table (daily from the Monarch import, then each sync); a date's balance is the
// latest one on or before it. months[] holds month-end balances and cumulative "added" for the charts.

func (a *app) investmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/investments", func(w http.ResponseWriter, r *http.Request) {
		out, err := a.investments(r.Context(), r.URL.Query().Get("period"))
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, out)
	})
}

type investmentAccount struct {
	ID          string  `json:"id"`
	Institution string  `json:"institution"`
	Name        string  `json:"name"`
	Start       int64   `json:"start"`
	End         int64   `json:"end"`
	Added       int64   `json:"added"`
	Dividends   int64   `json:"dividends"`
	Market      int64   `json:"market"`
	Months      []month `json:"months"`
}

type month struct {
	Month   string `json:"month"`
	Balance int64  `json:"balance"`
	Added   int64  `json:"added"` // cumulative since the period start
}

func (a *app) investments(ctx context.Context, period string) (map[string]any, error) {
	var last, first sql.NullString
	if err := a.db.QueryRowContext(ctx, `SELECT MAX(date), MIN(date) FROM balances b JOIN accounts a ON a.id=b.account_id WHERE a.type='investment'`).Scan(&last, &first); err != nil {
		return nil, err
	}
	if !last.Valid {
		return map[string]any{"period": period, "from": nil, "to": nil, "accounts": []investmentAccount{}}, nil
	}
	end, _ := time.Parse("2006-01-02", last.String)
	var from time.Time
	switch period {
	case "3m":
		from = end.AddDate(0, -3, 0)
	case "1y":
		from = end.AddDate(-1, 0, 0)
	case "all":
		from, _ = time.Parse("2006-01-02", first.String)
	default:
		period, from = "ytd", time.Date(end.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}
	fromS, endS := from.Format("2006-01-02"), end.Format("2006-01-02")

	rows, err := a.db.QueryContext(ctx, `SELECT id,institution,name FROM accounts WHERE type='investment' ORDER BY institution,name`)
	if err != nil {
		return nil, err
	}
	var list []investmentAccount
	for rows.Next() {
		var ia investmentAccount
		if err := rows.Scan(&ia.ID, &ia.Institution, &ia.Name); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, ia)
	}
	rows.Close()

	balanceAt := func(account, date string) (int64, error) {
		var v sql.NullInt64
		err := a.db.QueryRowContext(ctx, `SELECT current FROM balances WHERE account_id=$1 AND date<=$2 ORDER BY date DESC LIMIT 1`, account, date).Scan(&v)
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return v.Int64, err
	}
	for i := range list {
		ia := &list[i]
		// The balance just before the period starts, so a contribution on day one counts as added, not as start;
		// for "all" there is nothing before, so it is the first balance and that day's activity is left out.
		startDate := from.AddDate(0, 0, -1)
		if period == "all" {
			startDate = from
			fromS = from.AddDate(0, 0, 1).Format("2006-01-02")
		}
		if ia.Start, err = balanceAt(ia.ID, startDate.Format("2006-01-02")); err != nil {
			return nil, err
		}
		if ia.End, err = balanceAt(ia.ID, endS); err != nil {
			return nil, err
		}
		added := map[string]int64{} // by month
		rows, err := a.db.QueryContext(ctx, `SELECT date, kind, amount FROM investment_activity
			WHERE account_id=$1 AND date>=$2 AND date<=$3 AND kind IN ('contribution','income')`, ia.ID, fromS, endS)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var date, kind string
			var amount int64
			if err := rows.Scan(&date, &kind, &amount); err != nil {
				rows.Close()
				return nil, err
			}
			if kind == "income" {
				ia.Dividends += amount
			} else {
				ia.Added += amount
				added[date[:7]] += amount
			}
		}
		rows.Close()
		ia.Market = ia.End - ia.Start - ia.Added - ia.Dividends
		var cum int64
		for m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(end); m = m.AddDate(0, 1, 0) {
			monthEnd := m.AddDate(0, 1, -1)
			if monthEnd.After(end) {
				monthEnd = end
			}
			b, err := balanceAt(ia.ID, monthEnd.Format("2006-01-02"))
			if err != nil {
				return nil, err
			}
			cum += added[m.Format("2006-01")]
			ia.Months = append(ia.Months, month{m.Format("2006-01"), b, cum})
		}
	}
	return map[string]any{"period": period, "from": fromS, "to": endS, "accounts": list}, nil
}
