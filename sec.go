package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// SEC fund lookups (opt-in with SEC_USER_AGENT, which the SEC requires to hold a contact). After a sync, held
// symbols with no fresh split are looked up: a fund ticker resolves to its series, whose latest N-PORT filing
// lists every holding with its asset category and issuer country; an operating company's ticker is a stock.
// Only tickers are sent. Requests are spaced at most 5 a second, half the SEC's limit.

var secBase = "https://www.sec.gov"

var sec struct {
	sync.Mutex
	last   time.Time         // last request, for pacing
	at     time.Time         // when the ticker lists were fetched (kept a day)
	series map[string]string // fund ticker -> series id
	stocks map[string]bool   // operating company tickers
}

func secGet(ctx context.Context, path string) ([]byte, error) {
	sec.Lock()
	if wait := time.Until(sec.last.Add(200 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	sec.last = time.Now()
	sec.Unlock()
	req, err := http.NewRequestWithContext(ctx, "GET", secBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", os.Getenv("SEC_USER_AGENT"))
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("SEC %s: HTTP %d", path, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20)) // a total-market fund's filing runs to tens of MB
}

// secTickers loads the fund and company ticker lists, at most once a day.
func secTickers(ctx context.Context) (map[string]string, map[string]bool, error) {
	sec.Lock()
	series, stocks, fresh := sec.series, sec.stocks, time.Since(sec.at) < 24*time.Hour
	sec.Unlock()
	if fresh {
		return series, stocks, nil
	}
	body, err := secGet(ctx, "/files/company_tickers_mf.json")
	if err != nil {
		return nil, nil, err
	}
	var mf struct {
		Fields []string `json:"fields"`
		Data   [][]any  `json:"data"`
	}
	if err := json.Unmarshal(body, &mf); err != nil {
		return nil, nil, err
	}
	si, sy := slices.Index(mf.Fields, "seriesId"), slices.Index(mf.Fields, "symbol")
	if si < 0 || sy < 0 {
		return nil, nil, errors.New("SEC fund ticker list: unexpected fields")
	}
	series = map[string]string{}
	for _, row := range mf.Data {
		if len(row) > max(si, sy) {
			series[fmt.Sprint(row[sy])] = fmt.Sprint(row[si])
		}
	}
	if body, err = secGet(ctx, "/files/company_tickers.json"); err != nil {
		return nil, nil, err
	}
	var cos map[string]struct {
		Ticker string `json:"ticker"`
	}
	if err := json.Unmarshal(body, &cos); err != nil {
		return nil, nil, err
	}
	stocks = map[string]bool{}
	for _, c := range cos {
		stocks[c.Ticker] = true
	}
	sec.Lock()
	sec.series, sec.stocks, sec.at = series, stocks, time.Now()
	sec.Unlock()
	return series, stocks, nil
}

// nportSplit reads a series' latest NPORT-P and sums its holdings' percentages by class.
func nportSplit(ctx context.Context, seriesID string) (split [5]int, filed string, err error) {
	body, err := secGet(ctx, "/cgi-bin/browse-edgar?action=getcompany&type=NPORT-P&dateb=&owner=include&count=10&output=atom&CIK="+url.QueryEscape(seriesID))
	if err != nil {
		return split, "", err
	}
	type filing struct {
		Href string `xml:"content>filing-href"`
		Date string `xml:"content>filing-date"`
	}
	var feed struct {
		Entries []filing `xml:"entry"`
	}
	if err := secXML(body, &feed); err != nil {
		return split, "", err
	}
	if len(feed.Entries) == 0 {
		return split, "", errors.New("no NPORT-P filings")
	}
	newest := slices.MaxFunc(feed.Entries, func(x, y filing) int { return cmp.Compare(x.Date, y.Date) })
	u, err := url.Parse(newest.Href)
	if err != nil {
		return split, "", err
	}
	if body, err = secGet(ctx, u.Path[:strings.LastIndex(u.Path, "/")]+"/primary_doc.xml"); err != nil {
		return split, "", err
	}
	var doc struct {
		Holdings []struct {
			Pct     float64 `xml:"pctVal"`
			Cat     string  `xml:"assetCat"`
			CondCat struct {
				Cat string `xml:"assetCat,attr"`
			} `xml:"assetConditional"`
			Country string `xml:"invCountry"`
		} `xml:"formData>invstOrSecs>invstOrSec"`
	}
	if err := secXML(body, &doc); err != nil {
		return split, "", err
	}
	var pct [5]float64
	for _, h := range doc.Holdings {
		cat := cmp.Or(h.Cat, h.CondCat.Cat)
		i := 4
		switch {
		case cat == "EC" || cat == "EP":
			i = 1
			if h.Country == "US" {
				i = 0
			}
		case cat == "DBT" || cat == "LON" || cat == "SN" || strings.HasPrefix(cat, "ABS"):
			i = 2
		case cat == "STIV" || cat == "RA":
			i = 3
		}
		pct[i] += h.Pct
	}
	split, err = toBasisPoints(pct)
	return split, newest.Date, err
}

// secXML decodes EDGAR XML, whose Atom feeds declare ISO-8859-1 (Latin-1 bytes are their own code points).
func secXML(body []byte, v any) error {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.CharsetReader = func(label string, r io.Reader) (io.Reader, error) {
		if !strings.EqualFold(label, "ISO-8859-1") {
			return nil, fmt.Errorf("unsupported charset %q", label)
		}
		b, err := io.ReadAll(r)
		runes := make([]rune, len(b))
		for i, c := range b {
			runes[i] = rune(c)
		}
		return strings.NewReader(string(runes)), err
	}
	return d.Decode(v)
}

// toBasisPoints scales class totals to 10000, dropping net-short classes and giving the rounding to the largest.
func toBasisPoints(pct [5]float64) (split [5]int, err error) {
	var total float64
	for i := range pct {
		pct[i] = max(pct[i], 0)
		total += pct[i]
	}
	if total <= 0 {
		return split, errors.New("filing has no holdings")
	}
	sum, big := 0, 0
	for i, p := range pct {
		split[i] = int(p / total * 10000)
		sum += split[i]
		if p > pct[big] {
			big = i
		}
	}
	split[big] += 10000 - sum
	return split, nil
}

// fundLookups classifies held symbols that have no owner split and no fresh SEC one. Failures are logged and
// leave the symbol unclassified.
func (a *app) fundLookups(ctx context.Context) error {
	if os.Getenv("SEC_USER_AGENT") == "" {
		return nil
	}
	classes, err := a.fundClasses(ctx)
	if err != nil {
		return err
	}
	var symbols []string
	rows, err := a.db.QueryContext(ctx, `SELECT raw FROM holdings`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		if p, ok := parseHolding(raw); ok && !p.Unvested && p.Symbol != "" && classify(p, classes, time.Now()) == nil && !slices.Contains(symbols, p.Symbol) {
			symbols = append(symbols, p.Symbol)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(symbols) == 0 {
		return err
	}
	a.simplefin.progress.set("Looking up fund holdings", 0, 0)
	series, stocks, err := secTickers(ctx)
	if err != nil {
		return err
	}
	for _, sym := range symbols {
		var split [5]int
		source, filed := "sec", ""
		switch id, ok := series[sym]; {
		case ok:
			if split, filed, err = nportSplit(ctx, id); err != nil {
				log.Printf("sec: %s: %v", sym, err)
				continue
			}
		case stocks[sym]:
			split, source = [5]int{10000, 0, 0, 0, 0}, "stock" // US by default; the owner can override
		default:
			log.Printf("sec: %s is not in the SEC's ticker lists; classify it in the app", sym)
			continue
		}
		if _, err := a.db.ExecContext(ctx, `INSERT INTO fund_classes(symbol,us_stock,intl_stock,bonds,cash,other,source,as_of) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT(symbol) DO UPDATE SET us_stock=$2,intl_stock=$3,bonds=$4,cash=$5,other=$6,source=$7,as_of=$8,saved_at=now() WHERE fund_classes.source<>'owner'`,
			sym, split[0], split[1], split[2], split[3], split[4], source, filed); err != nil {
			return err
		}
	}
	return nil
}
