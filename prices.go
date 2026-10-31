package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// GET /api/prices?range=1d|5d|1mo|3mo|ytd|1y|5y|max[&compare=SPY]: today's holdings priced over the range, from
// Yahoo Finance's public chart endpoint (no key; unofficial, so it can change or rate-limit).
//
// A mutual fund prices once a day, after the close. For bars shorter than a day it follows its ETF share class
// (the same portfolio), scaled from the fund's latest price and that ETF's price at the same moment; for daily
// bars it uses its own prices. A fund with no public ticker (a 401(k) trust) follows an ETF only when its name says
// it tracks the S&P 500, scaled from its value at the last sync; otherwise it, and money market funds, stay flat.
// Shares are today's for the whole range: a longer range shows how today's portfolio did, not past balances.

var priceIntervals = map[string]string{"1d": "1m", "5d": "5m", "1mo": "1h", "3mo": "1d", "ytd": "1d", "1y": "1d", "5y": "1wk", "max": "1mo"}

// etfTwin maps index mutual funds to the ETF share class of the same fund (or one tracking the same index).
var etfTwin = map[string]string{
	"VFIAX": "VOO", "VTSAX": "VTI", "VTIAX": "VXUS", "VEXAX": "VXF", "VSIAX": "VBR", "VVIAX": "VTV", "VIGAX": "VUG",
	"VIMAX": "VO", "VSMAX": "VB", "VFSAX": "VSS", "VFWAX": "VEU", "VEMAX": "VWO", "VTMGX": "VEA", "VGSLX": "VNQ",
	"VBTLX": "BND", "VTABX": "BNDX", "VTEAX": "VTEB", "VDADX": "VIG", "VHYAX": "VYM", "VWIUX": "VTEB",
	"FXAIX": "IVV", "SWPPX": "IVV", "FSKAX": "ITOT", "SWTSX": "ITOT", "FTIHX": "IXUS",
}

var sp500Name = regexp.MustCompile(`(?i)\b(s&p )?500\b.*\b(idx|index)\b`)

type yChart struct {
	ts        []int64
	close     []float64 // NaN where Yahoo has no trade
	prevClose float64   // close before the range
	price     float64   // latest price (a fund's latest NAV)
	priceTime int64
	gmtoff    int64
	fund      bool
}

var (
	yahooURL   = "https://query1.finance.yahoo.com/v8/finance/chart/"
	yahooCache sync.Map // symbol|range|interval -> yCached
)

type yCached struct {
	at    time.Time
	chart yChart
	err   error
}

// yahoo fetches one symbol's bars, cached for a minute (intraday) or an hour, failures included, to stay polite.
func yahoo(ctx context.Context, symbol, rng, interval string) (yChart, error) {
	key := symbol + "|" + rng + "|" + interval
	ttl := time.Hour
	if strings.HasSuffix(interval, "m") || interval == "1h" {
		ttl = time.Minute
	}
	if c, ok := yahooCache.Load(key); ok && time.Since(c.(yCached).at) < ttl {
		return c.(yCached).chart, c.(yCached).err
	}
	ch, err := fetchYahoo(ctx, symbol, rng, interval)
	yahooCache.Store(key, yCached{time.Now(), ch, err})
	return ch, err
}

func fetchYahoo(ctx context.Context, symbol, rng, interval string) (yChart, error) {
	var ch yChart
	u := yahooURL + url.PathEscape(symbol) + "?" + url.Values{"range": {rng}, "interval": {interval}}.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return ch, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (money-tracker)") // Yahoo refuses requests without one
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return ch, err
	}
	defer resp.Body.Close()
	var body struct {
		Chart struct {
			Result []struct {
				Meta struct {
					InstrumentType     string  `json:"instrumentType"`
					RegularMarketPrice float64 `json:"regularMarketPrice"`
					RegularMarketTime  int64   `json:"regularMarketTime"`
					ChartPreviousClose float64 `json:"chartPreviousClose"`
					GMTOffset          int64   `json:"gmtoffset"`
				} `json:"meta"`
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Quote []struct {
						Close []*float64 `json:"close"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
		} `json:"chart"`
	}
	if resp.StatusCode != 200 {
		return ch, fmt.Errorf("yahoo %s: %s", symbol, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ch, fmt.Errorf("yahoo %s: %w", symbol, err)
	}
	if len(body.Chart.Result) == 0 {
		return ch, fmt.Errorf("yahoo %s: no data", symbol)
	}
	r := body.Chart.Result[0]
	ch = yChart{ts: r.Timestamp, prevClose: r.Meta.ChartPreviousClose, price: r.Meta.RegularMarketPrice,
		priceTime: r.Meta.RegularMarketTime, gmtoff: r.Meta.GMTOffset, fund: r.Meta.InstrumentType == "MUTUALFUND"}
	var closes []*float64
	if len(r.Indicators.Quote) > 0 {
		closes = r.Indicators.Quote[0].Close
	}
	ch.close = make([]float64, len(ch.ts))
	for i := range ch.ts {
		ch.close[i] = math.NaN()
		if i < len(closes) && closes[i] != nil {
			ch.close[i] = *closes[i]
		}
	}
	return ch, nil
}

// at returns the last close at or before t, or the close before the range when there is none.
func (c yChart) at(t int64) float64 {
	p := c.prevClose
	for i, ts := range c.ts {
		if ts > t {
			break
		}
		if !math.IsNaN(c.close[i]) {
			p = c.close[i]
		}
	}
	return p
}

// key puts a bar on the shared time axis: its own time for intraday bars, its exchange-local date otherwise, so
// a fund's and an ETF's daily bars line up.
func (c yChart) key(i int, daily bool) int64 {
	if !daily {
		return c.ts[i]
	}
	d := time.Unix(c.ts[i]+c.gmtoff, 0).UTC()
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

var tickerRe = regexp.MustCompile(`^[A-Za-z0-9.^=-]{1,12}$`)

func (a *app) priceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/prices", func(w http.ResponseWriter, r *http.Request) {
		rng, compare := r.URL.Query().Get("range"), strings.ToUpper(r.URL.Query().Get("compare"))
		if _, ok := priceIntervals[rng]; !ok {
			jsonResponse(w, 400, map[string]string{"error": "range must be one of 1d, 5d, 1mo, 3mo, ytd, 1y, 5y, max"})
			return
		}
		if compare != "" && !tickerRe.MatchString(compare) {
			jsonResponse(w, 400, map[string]string{"error": "compare must be a ticker symbol"})
			return
		}
		out, err := a.prices(r.Context(), rng, compare)
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, out)
	})
}

type pricedHolding struct {
	AccountID   string     `json:"account_id"`
	Account     string     `json:"account"`
	Symbol      string     `json:"symbol"`
	Description string     `json:"description"`
	Class       *fundClass `json:"class,omitempty"`
	Source      string     `json:"source"` // live, daily (a fund's own daily prices), twin:<ETF>, or flat
	Base        int64      `json:"base"`   // value at the close before the range, cents
	Values      []int64    `json:"values"` // cents, one per time
}

// series is how one holding is priced: value(t) = scale × chart's close at t.
type series struct {
	h     *pricedHolding
	chart *yChart
	scale float64
}

func (a *app) prices(ctx context.Context, rng, compare string) (map[string]any, error) {
	interval := priceIntervals[rng]
	daily := !strings.HasSuffix(interval, "m") && interval != "1h"
	classes, err := a.fundClasses(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT h.account_id,a.name,h.raw,h.synced_at FROM holdings h JOIN accounts a ON a.id=h.account_id WHERE a.type='investment'`)
	if err != nil {
		return nil, err
	}
	type held struct {
		position
		account, name string
		synced        time.Time
	}
	var list []held
	for rows.Next() {
		var h held
		var raw []byte
		if err := rows.Scan(&h.account, &h.name, &raw, &h.synced); err != nil {
			rows.Close()
			return nil, err
		}
		if p, ok := parseHolding(raw); ok && !p.Unvested {
			h.position = p
			list = append(list, h)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fetch every symbol needed at once: each holding's own, its ETF twin for intraday bars, and the comparison.
	charts := map[string]*yChart{}
	var cmpErr error
	want := map[string]bool{}
	for _, h := range list {
		if h.Symbol != "" && classify(h.position, nil, time.Now()) != moneyMarket {
			want[h.Symbol] = true
			if t := etfTwin[strings.ToUpper(h.Symbol)]; t != "" && !daily {
				want[t] = true
			}
		}
	}
	want["VOO"] = want["VOO"] || slices.ContainsFunc(list, func(h held) bool { return sp500Name.MatchString(h.Description) })
	if compare != "" {
		want[compare] = true
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for s := range want {
		if !want[s] {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := yahoo(ctx, s, rng, interval)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if s == compare {
					cmpErr = err
				}
				return
			}
			charts[s] = &c
		}()
	}
	wg.Wait()

	var out []*pricedHolding
	var ss []series
	now := time.Now()
	for _, h := range list {
		ph := &pricedHolding{AccountID: h.account, Account: h.name, Symbol: h.Symbol, Description: h.Description, Class: classify(h.position, classes, now), Source: "flat"}
		out = append(out, ph)
		sym := strings.ToUpper(h.Symbol)
		own, twin := charts[h.Symbol], charts[etfTwin[sym]]
		switch {
		case ph.Class == moneyMarket:
		case own != nil && own.fund && !daily && twin != nil:
			// Follow the ETF from the fund's latest NAV: the fund is worth shares × NAV when the ETF was at twin.at(navTime).
			if p := twin.at(own.priceTime); p > 0 {
				ss = append(ss, series{ph, twin, h.Shares * own.price / p})
				ph.Source = "twin:" + etfTwin[sym]
			}
		case own != nil && !(own.fund && !daily):
			ss = append(ss, series{ph, own, h.Shares})
			ph.Source = map[bool]string{true: "daily", false: "live"}[own.fund]
		case own == nil && charts["VOO"] != nil && sp500Name.MatchString(h.Description):
			if p := charts["VOO"].at(h.synced.Unix()); p > 0 {
				ss = append(ss, series{ph, charts["VOO"], float64(h.Value) / 100 / p})
				ph.Source = "twin:VOO"
			}
		}
		ph.Base = h.Value
	}

	// The shared time axis: every priced bar.
	seen := map[int64]bool{}
	for _, s := range ss {
		for i := range s.chart.ts {
			if !math.IsNaN(s.chart.close[i]) {
				seen[s.chart.key(i, daily)] = true
			}
		}
	}
	times := make([]int64, 0, len(seen))
	for t := range seen {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	for _, s := range ss {
		s.h.Base = int64(math.Round(s.scale * s.chart.prevClose * 100))
		s.h.Values = fill(s.chart, times, daily, s.scale)
	}
	for _, h := range out {
		if h.Source == "flat" {
			h.Values = make([]int64, len(times))
			for i := range h.Values {
				h.Values[i] = h.Base
			}
		}
	}
	res := map[string]any{"range": rng, "interval": interval, "daily": daily, "times": times, "holdings": out}
	if compare != "" {
		if c := charts[compare]; c != nil {
			res["compare"] = map[string]any{"symbol": compare, "base": int64(math.Round(c.prevClose * 100)), "values": fill(c, times, daily, 1)}
		} else {
			res["compare_error"] = fmt.Sprint(cmpErr)
		}
	}
	failed := []string{} // held symbols with no price, so a flat line isn't mistaken for a quiet market
	for _, h := range out {
		if h.Source == "flat" && h.Class != moneyMarket && h.Symbol != "" && !slices.Contains(failed, h.Symbol) {
			failed = append(failed, h.Symbol)
		}
	}
	res["unavailable"] = failed
	return res, nil
}

// fill gives the chart's value (scale × close, in cents) at each time, carrying the last close forward and
// starting from the close before the range.
func fill(c *yChart, times []int64, daily bool, scale float64) []int64 {
	out := make([]int64, len(times))
	p, j := c.prevClose, 0
	for i, t := range times {
		for j < len(c.ts) && c.key(j, daily) <= t {
			if !math.IsNaN(c.close[j]) {
				p = c.close[j]
			}
			j++
		}
		out[i] = int64(math.Round(scale * p * 100))
	}
	return out
}
