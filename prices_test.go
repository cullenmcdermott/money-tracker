package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestPrices(t *testing.T) {
	const t0, t1, t2 = 1700000000, 1700000060, 1700000120
	charts := map[string]string{
		// A mutual fund: its latest NAV (500) was struck when its ETF twin traded at 400.
		"VFIAX": fmt.Sprintf(`{"meta":{"instrumentType":"MUTUALFUND","regularMarketPrice":500,"regularMarketTime":%d,"chartPreviousClose":499},"timestamp":[],"indicators":{"quote":[{"close":[]}]}}`, t0),
		"VOO":   fmt.Sprintf(`{"meta":{"instrumentType":"ETF","chartPreviousClose":400},"timestamp":[%d,%d,%d],"indicators":{"quote":[{"close":[400,404,null]}]}}`, t0, t1, t2),
		"ABC":   fmt.Sprintf(`{"meta":{"instrumentType":"EQUITY","chartPreviousClose":19},"timestamp":[%d,%d],"indicators":{"quote":[{"close":[20,21]}]}}`, t1, t2),
	}
	yahooSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := charts[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok || r.Header.Get("User-Agent") == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"chart":{"result":[%s]}}`, c)
	}))
	defer yahooSrv.Close()
	defer func(u string) { yahooURL = u }(yahooURL)
	yahooURL = yahooSrv.URL + "/"

	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db}
	if _, err := db.Exec(fmt.Sprintf(`INSERT INTO items(id) VALUES('item');
		INSERT INTO accounts(id,item_id,name,guessed_type) VALUES('brk','item','Brokerage','investment'),('k','item','401k','investment');
		INSERT INTO holdings(account_id,raw,synced_at) VALUES
			('brk','{"symbol":"VFIAX","description":"Acme 500 Index Admiral","shares":"10","market_value":"4990"}',to_timestamp(%[1]d)),
			('brk','{"symbol":"ABC","description":"Abc Corp","shares":"5","market_value":"100"}',to_timestamp(%[1]d)),
			('brk','{"symbol":"VMFXX","description":"Acme Federal Money Market Fund","shares":"50","market_value":"50"}',to_timestamp(%[1]d)),
			('k','{"symbol":"ZZTR1","description":"ACME INST 500 IDX TR","shares":"3","market_value":"1000"}',to_timestamp(%[1]d)),
			('k','{"symbol":"QQQQX","description":"Acme Target 2055 Trust","shares":"3","market_value":"700"}',to_timestamp(%[1]d))`, t0)); err != nil {
		t.Fatal(err)
	}
	out, err := a.prices(context.Background(), "1d", "ABC")
	if err != nil {
		t.Fatal(err)
	}
	if got := out["times"].([]int64); !reflect.DeepEqual(got, []int64{t0, t1, t2}) {
		t.Errorf("times %v", got)
	}
	got := map[string]string{}
	for _, h := range out["holdings"].([]*pricedHolding) {
		got[h.Symbol] = fmt.Sprint(h.Source, h.Base, h.Values)
	}
	want := map[string]string{
		"VFIAX": "twin:VOO500000 [500000 505000 505000]", // 10 shares × $500 NAV, then moving with VOO, carried over its gap
		"ABC":   "live9500 [9500 10000 10500]",           // from the close before the range until its first trade
		"VMFXX": "flat5000 [5000 5000 5000]",
		"ZZTR1": "twin:VOO100000 [100000 101000 101000]", // no public price, but its name says S&P 500: VOO from the synced value
		"QQQQX": "flat70000 [70000 70000 70000]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("holdings\n got %v\nwant %v", got, want)
	}
	if cmp := fmt.Sprint(out["compare"]); cmp != "map[base:1900 symbol:ABC values:[1900 2000 2100]]" {
		t.Errorf("compare %s", cmp)
	}
	if u := fmt.Sprint(out["unavailable"]); u != "[QQQQX]" {
		t.Errorf("unavailable %s", u)
	}
}
