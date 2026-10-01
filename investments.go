package main

import (
	"cmp"
	"context"
	"database/sql"
	"net/http"
	"slices"
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
	// The owner's split for a symbol, as five whole percentages (US stock, international stock, bonds, cash,
	// other) totalling 100. It replaces any SEC split and is never looked up again until cleared.
	mux.HandleFunc("PUT /api/fund-classes/{symbol}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Split [5]int `json:"split"`
		}
		if !decode(w, r, &body) {
			return
		}
		sum := 0
		for _, v := range body.Split {
			if v < 0 {
				sum = -1
				break
			}
			sum += v
		}
		if sum != 100 {
			jsonResponse(w, 400, map[string]string{"error": "Make the percentages total 100."})
			return
		}
		s := body.Split
		_, err := a.db.ExecContext(r.Context(), `INSERT INTO fund_classes(symbol,us_stock,intl_stock,bonds,cash,other,source) VALUES($1,$2,$3,$4,$5,$6,'owner')
			ON CONFLICT(symbol) DO UPDATE SET us_stock=$2,intl_stock=$3,bonds=$4,cash=$5,other=$6,source='owner',as_of='',saved_at=now()`,
			r.PathValue("symbol"), s[0]*100, s[1]*100, s[2]*100, s[3]*100, s[4]*100)
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"saved": true})
	})
	mux.HandleFunc("DELETE /api/fund-classes/{symbol}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := a.db.ExecContext(r.Context(), `DELETE FROM fund_classes WHERE symbol=$1 AND source='owner'`, r.PathValue("symbol")); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"saved": true})
	})
}

func (a *app) fundClasses(ctx context.Context) (map[string]fundClass, error) {
	out := map[string]fundClass{}
	rows, err := a.db.QueryContext(ctx, `SELECT symbol,us_stock,intl_stock,bonds,cash,other,source,as_of,saved_at FROM fund_classes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sym string
		var c fundClass
		if err := rows.Scan(&sym, &c.Split[0], &c.Split[1], &c.Split[2], &c.Split[3], &c.Split[4], &c.Source, &c.AsOf, &c.saved); err != nil {
			return nil, err
		}
		out[sym] = c
	}
	return out, rows.Err()
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
	// Latest synced holdings (none when the brokerage sends none). Cost and Gain sum the positions with a
	// known cost; NoCost is the value of those without. Unvested awards are kept apart from all of these.
	Positions  []position `json:"positions,omitempty"`
	Cost       int64      `json:"cost"`
	Gain       int64      `json:"unrealized"` // "gain" is taken by the page for dividends plus market change
	NoCost     int64      `json:"no_cost"`
	Unvested   int64      `json:"unvested"`
	UnvestedN  int        `json:"unvested_count"`
	HoldingsAt string     `json:"holdings_at,omitempty"`
	Allocation allocation `json:"allocation"`
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
	portfolio, err := a.addPositions(ctx, list)
	if err != nil {
		return nil, err
	}
	return map[string]any{"period": period, "from": fromS, "to": endS, "accounts": list, "allocation": portfolio}, nil
}

// addPositions fills each account's holdings, largest first, its cost, gain and unvested totals and its
// allocation, and returns the allocation of all of them together.
func (a *app) addPositions(ctx context.Context, list []investmentAccount) (allocation, error) {
	var all allocation
	idx := map[string]*investmentAccount{}
	for i := range list {
		idx[list[i].ID] = &list[i]
	}
	classes, err := a.fundClasses(ctx)
	if err != nil {
		return all, err
	}
	now := time.Now()
	rows, err := a.db.QueryContext(ctx, `SELECT account_id,raw,to_char(synced_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM holdings `)
	if err != nil {
		return all, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, at string
		var raw []byte
		if err := rows.Scan(&id, &raw, &at); err != nil {
			return all, err
		}
		ia := idx[id]
		if ia == nil {
			continue
		}
		p, ok := parseHolding(raw)
		if !ok {
			continue
		}
		ia.HoldingsAt = at
		switch {
		case p.Unvested:
			ia.Unvested += p.Value
			ia.UnvestedN++
			continue
		case p.Cost != nil:
			ia.Cost += *p.Cost
			ia.Gain += p.Value - *p.Cost
		default:
			ia.NoCost += p.Value
		}
		p.Class = classify(p, classes, now)
		ia.Allocation.add(p.Value, p.Class)
		all.add(p.Value, p.Class)
		ia.Positions = append(ia.Positions, p)
	}
	for _, ia := range idx {
		slices.SortStableFunc(ia.Positions, func(x, y position) int { return cmp.Compare(y.Value, x.Value) })
	}
	return all, rows.Err()
}
