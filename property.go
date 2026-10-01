package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A home's value comes from the Ada County Assessor's public parcel layer: the total assessed value, which Idaho sets
// at market value each January. Only the parcel number is sent. When the assessed value changes, it becomes the
// account's balance from that day on (decided 2026-09-30: the county value, not a Zillow estimate).
var assessorURL = "https://services2.arcgis.com/dgGjZc6xAH5m5JyP/arcgis/rest/services/Parcels/FeatureServer/5/query"

type execQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func assessedValue(ctx context.Context, parcel string) (cents int64, year int, err error) {
	q := url.Values{"where": {"PARCEL='" + strings.ReplaceAll(parcel, "'", "") + "'"}, "outFields": {"TOTALVALUE,PROPYEAR"}, "returnGeometry": {"false"}, "f": {"json"}}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assessorURL+"?"+q.Encode(), nil)
	if err != nil {
		return 0, 0, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("assessor request failed: %w", err)
	}
	defer res.Body.Close()
	var body struct {
		Features []struct {
			Attributes struct {
				TotalValue float64 `json:"TOTALVALUE"`
				PropYear   int     `json:"PROPYEAR"`
			} `json:"attributes"`
		} `json:"features"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return 0, 0, fmt.Errorf("assessor response: %w", err)
	}
	if len(body.Features) != 1 || body.Features[0].Attributes.TotalValue <= 0 {
		return 0, 0, errors.New("assessor has no value for parcel " + parcel)
	}
	a := body.Features[0].Attributes
	return int64(a.TotalValue * 100), a.PropYear, nil
}

// refreshProperty records the parcel's assessed value as today's balance when it differs from the current one, and
// returns a one-line summary. Failures are reported, never fatal.
func refreshProperty(ctx context.Context, db execQuerier, accountID, parcel string) string {
	cents, year, err := assessedValue(ctx, parcel)
	if err != nil {
		return "Home value not refreshed: " + err.Error()
	}
	var current sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT current FROM accounts WHERE id=$1`, accountID).Scan(&current); err != nil {
		return "Home value not refreshed: " + err.Error()
	}
	if current.Valid && current.Int64 == cents {
		return fmt.Sprintf("Home value unchanged: %s assessed value $%d.", fmt.Sprint(year), cents/100)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if _, err := db.ExecContext(ctx, `UPDATE accounts SET current=$1 WHERE id=$2`, cents, accountID); err != nil {
		return "Home value not refreshed: " + err.Error()
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO balances(account_id,date,current) VALUES($1,$2,$3) ON CONFLICT(account_id,date) DO UPDATE SET current=excluded.current`, accountID, today, cents); err != nil {
		return "Home value not refreshed: " + err.Error()
	}
	return fmt.Sprintf("Home value set to the %d assessed value, $%d, from %s.", year, cents/100, today)
}

// propertyLoop re-checks every property account once a day; the value changes about once a year.
func (a *app) propertyLoop(ctx context.Context) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		rows, err := a.db.QueryContext(ctx, `SELECT id,parcel FROM accounts WHERE parcel<>''`)
		if err != nil {
			log.Printf("property refresh: %v", err)
			continue
		}
		var list [][2]string
		for rows.Next() {
			var id, parcel string
			if rows.Scan(&id, &parcel) == nil {
				list = append(list, [2]string{id, parcel})
			}
		}
		rows.Close()
		for _, p := range list {
			log.Printf("property %s: %s", p[0], refreshProperty(ctx, a.db, p[0], p[1]))
		}
	}
}
