package main

import (
	"bytes"
	"encoding/json"
	"log"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// position is one holding from the latest sync. Cost is the total cost in cents, nil when the brokerage sent
// none or zero (zero would make a money market fund's whole value look like gain).
type position struct {
	Symbol      string     `json:"symbol"`
	Description string     `json:"description"`
	Shares      float64    `json:"shares"`
	Value       int64      `json:"value"`
	Cost        *int64     `json:"cost,omitempty"`
	Unvested    bool       `json:"unvested,omitempty"`
	Class       *fundClass `json:"class,omitempty"` // nil = unclassified
}

// Unvested awards come as zero shares with a value; the account balance leaves them out.
var unvestedDesc = regexp.MustCompile(`(?i)restricted ?stock|\brsu\b`)

// parseHolding reads one raw Bridge holding. Fields arrive as decimal strings (sometimes numbers), with any
// number of decimal places; cost_basis is per share. ok is false for holdings to leave out.
func parseHolding(raw []byte) (p position, ok bool) {
	var h struct {
		Symbol      string      `json:"symbol"`
		Description string      `json:"description"`
		Shares      json.Number `json:"shares"`
		Value       json.Number `json:"market_value"`
		CostBasis   json.Number `json:"cost_basis"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&h); err != nil {
		log.Printf("holdings: skipping unreadable holding: %v", err)
		return p, false
	}
	shares, okS := new(big.Rat).SetString(h.Shares.String())
	value, okV := new(big.Rat).SetString(h.Value.String())
	if !okS || !okV {
		log.Printf("holdings: skipping %q: shares %q, value %q", h.Symbol, h.Shares, h.Value)
		return p, false
	}
	p = position{Symbol: h.Symbol, Description: h.Description, Value: ratCents(value)}
	p.Shares, _ = shares.Float64()
	if shares.Sign() == 0 {
		if p.Value > 0 && unvestedDesc.MatchString(h.Description) {
			p.Unvested = true
			return p, true
		}
		if p.Value != 0 {
			log.Printf("holdings: skipping %q: zero shares worth %d cents", h.Symbol, p.Value)
		}
		return p, false
	}
	if c, ok := new(big.Rat).SetString(h.CostBasis.String()); ok && c.Sign() > 0 {
		total := ratCents(c.Mul(c, shares))
		p.Cost = &total
	}
	return p, true
}

// ratCents rounds dollars to cents, half away from zero.
func ratCents(r *big.Rat) int64 {
	c := new(big.Rat).Mul(r, big.NewRat(100, 1))
	n, rem := new(big.Int).QuoRem(c.Num(), c.Denom(), new(big.Int))
	if rem.Abs(rem).Mul(rem, big.NewInt(2)).Cmp(c.Denom()) >= 0 {
		n.Add(n, big.NewInt(int64(c.Sign())))
	}
	return n.Int64()
}

// fundClass is a symbol's split in basis points: US stock, international stock, bonds, cash, other.
type fundClass struct {
	Split  [5]int `json:"split"`
	Source string `json:"source"` // owner, sec, stock, or money_market (not stored)
	AsOf   string `json:"as_of,omitempty"`
	saved  time.Time
}

const secFresh = 120 * 24 * time.Hour // about one N-PORT quarter plus its 60-day publication lag

var moneyMarket = &fundClass{Split: [5]int{0, 0, 0, 10000, 0}, Source: "money_market"}

// classify returns a position's split, or nil when it is unclassified: the owner's split first, then money
// market funds as cash, then a saved SEC (or operating company) split, as long as an SEC one is fresh.
func classify(p position, classes map[string]fundClass, now time.Time) *fundClass {
	c, ok := classes[p.Symbol]
	switch {
	case ok && c.Source == "owner":
		return &c
	// ponytail: US money market tickers end in XX by convention; N-PORT can't say, as these funds file N-MFP.
	case len(p.Symbol) == 5 && strings.HasSuffix(strings.ToUpper(p.Symbol), "XX"), strings.Contains(strings.ToLower(p.Description), "money market"):
		return moneyMarket
	case ok && (c.Source == "stock" || now.Sub(c.saved) < secFresh):
		return &c
	}
	return nil
}

// allocation is invested value by class, in cents.
type allocation struct {
	USStock      int64 `json:"us_stock"`
	IntlStock    int64 `json:"intl_stock"`
	Bonds        int64 `json:"bonds"`
	Cash         int64 `json:"cash"`
	Other        int64 `json:"other"`
	Unclassified int64 `json:"unclassified"`
}

func (al *allocation) add(value int64, c *fundClass) {
	if c == nil {
		al.Unclassified += value
		return
	}
	for i, f := range []*int64{&al.USStock, &al.IntlStock, &al.Bonds, &al.Cash, &al.Other} {
		*f += value * int64(c.Split[i]) / 10000
	}
}
