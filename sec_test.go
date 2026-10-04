package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSEC serves trimmed, invented EDGAR responses and records each request's path and user agent.
func fakeSEC(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RequestURI()+" "+r.UserAgent())
		mu.Unlock()
		switch r.URL.Path {
		case "/files/company_tickers_mf.json":
			fmt.Fprint(w, `{"fields":["cik","seriesId","classId","symbol"],"data":[[1001,"S000000001","C000000001","ABCDX"],[1001,"S000000002","C000000002","QRSTX"]]}`)
		case "/files/company_tickers.json":
			fmt.Fprint(w, `{"0":{"cik_str":2002,"ticker":"ACME","title":"Acme Corp"}}`)
		case "/cgi-bin/browse-edgar":
			if r.URL.Query().Get("CIK") != "S000000001" {
				fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"></feed>`)
				return
			}
			fmt.Fprintf(w, `<?xml version="1.0" encoding="ISO-8859-1" ?><feed xmlns="http://www.w3.org/2005/Atom">
				<entry><content type="text/xml"><filing-date>2026-05-28</filing-date><filing-href>%[1]s/Archives/edgar/data/1001/000100126000001/0001001-26-000001-index.htm</filing-href></content></entry>
				<entry><content type="text/xml"><filing-date>2026-08-27</filing-date><filing-href>%[1]s/Archives/edgar/data/1001/000100126000002/0001001-26-000002-index.htm</filing-href></content></entry>
				</feed>`, srv.URL)
		case "/Archives/edgar/data/1001/000100126000002/primary_doc.xml":
			fmt.Fprint(w, `<?xml version="1.0"?><edgarSubmission xmlns="http://www.sec.gov/edgar/nport"><formData><invstOrSecs>
				<invstOrSec><name>Example Widgets</name><pctVal>60.5</pctVal><assetCat>EC</assetCat><invCountry>US</invCountry></invstOrSec>
				<invstOrSec><name>Sample Gadgets</name><pctVal>36.5</pctVal><assetCat>EC</assetCat><invCountry>US</invCountry></invstOrSec>
				<invstOrSec><name>Cash Fund</name><pctVal>3</pctVal><assetCat>STIV</assetCat><invCountry>US</invCountry></invstOrSec>
				<invstOrSec><name>Future</name><pctVal>-0.2</pctVal><assetConditional assetCat="DFE"/><invCountry>US</invCountry></invstOrSec>
				</invstOrSecs></formData></edgarSubmission>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := secBase
	secBase = srv.URL
	t.Cleanup(func() { secBase = old })
	sec.Lock()
	sec.at, sec.series, sec.stocks = time.Time{}, nil, nil
	sec.Unlock()
	return &seen
}

func TestFundLookups(t *testing.T) {
	a := holdingsApp(t)
	if _, err := a.db.Exec(`INSERT INTO holdings(account_id,raw) VALUES
		('ira','{"symbol":"ACME","shares":"10","market_value":"1000"}'),
		('ira','{"symbol":"CT401","description":"Example Target 2050 Trust","shares":"10","market_value":"1000"}'),
		('ira','{"symbol":"QRSTX","shares":"10","market_value":"1000"}')`); err != nil {
		t.Fatal(err)
	}
	seen := fakeSEC(t)

	t.Setenv("SEC_USER_AGENT", "")
	if err := a.fundLookups(context.Background()); err != nil || len(*seen) != 0 {
		t.Fatalf("lookups off: %v, requests %v", err, *seen)
	}

	t.Setenv("SEC_USER_AGENT", "Example Owner owner@example.test")
	if err := a.fundLookups(context.Background()); err != nil {
		t.Fatal(err)
	}
	classes, err := a.fundClasses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c := classes["ABCDX"]; c.Source != "sec" || c.AsOf != "2026-08-27" || c.Split != [5]int{9700, 0, 0, 300, 0} {
		t.Errorf("fund = %+v, want 97%% US stock, 3%% cash from the newest filing", c)
	}
	if c := classes["ACME"]; c.Source != "stock" || c.Split != [5]int{10000, 0, 0, 0, 0} {
		t.Errorf("operating company = %+v", c)
	}
	for _, sym := range []string{"CT401", "QRSTX", "EXMXX"} { // not listed; no filings; money market needs no lookup
		if c, ok := classes[sym]; ok {
			t.Errorf("%s = %+v, want unclassified", sym, c)
		}
	}
	for _, s := range *seen {
		if !strings.HasSuffix(s, " Example Owner owner@example.test") {
			t.Errorf("request without the configured user agent: %q", s)
		}
	}

	// Fresh splits and owner splits are not looked up again.
	if _, err := a.db.Exec(`INSERT INTO fund_classes(symbol,us_stock,source) VALUES('CT401',10000,'owner')`); err != nil {
		t.Fatal(err)
	}
	*seen = nil
	if err := a.fundLookups(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range *seen { // only QRSTX, still without a filing, is tried again; the ticker lists are cached
		if strings.HasPrefix(s, "/files/") || strings.Contains(s, "S000000001") {
			t.Errorf("unexpected request %q", s)
		}
	}
}
