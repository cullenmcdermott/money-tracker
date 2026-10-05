package main

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The Plan page: a yearly forecast, in today's dollars, of the money in the owner's cash and investment
// accounts, built from their own last 12 months, balances and contributions (planBaseline), plus whatever they
// change (planDoc), projected by project().

type planAccount struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Institution    string  `json:"institution"`
	Type           string  `json:"type"`
	Balance        int64   `json:"balance"`
	Include        bool    `json:"include"`
	Bucket         string  `json:"bucket"` // cash, taxable, pretax or roth
	Growth         float64 `json:"growth"` // nominal yearly %, before inflation
	Contribution   int64   `json:"contribution"`
	PenaltyFreeAge float64 `json:"penalty_free_age"` // withdrawals before this age pay the early penalty (pre-tax and Roth)
	// What the data shows behind Contribution and Growth, a year: Deposits (payroll and other money from outside
	// the owner's accounts) and ESPP sales make up Contribution; RSU vest sales, unclear stock sales and moves
	// between own accounts are left out of it; Interest, paid into a cash account, sets its Growth.
	Deposits int64 `json:"deposits"`
	ESPP     int64 `json:"espp"`
	RSU      int64 `json:"rsu"`
	Unclear  int64 `json:"unclear"`
	Internal int64 `json:"internal"`
	Interest int64 `json:"interest"`
}

// planBaseline is what the data says, before the owner changes anything. Amounts in cents.
type planBaseline struct {
	Months   int   `json:"months"`         // complete months behind the averages, up to 12
	Income   int64 `json:"income_monthly"` // without interest, which is in the accounts' growth
	Spending int64 `json:"spending_monthly"`
	Interest int64 `json:"interest_monthly"`
	Unvested int64 `json:"unvested"`
	// The largest sources behind Income, a year each, so the owner can spot money that came from their own
	// accounts (an untracked brokerage, say) and mark it as a transfer.
	IncomeSources []planSource     `json:"income_sources"`
	UnclearSales  []planEquitySale `json:"unclear_sales"` // stock sales tied to neither an RSU vest nor an ESPP purchase
	Accounts      []planAccount    `json:"accounts"`
}

type planSource struct {
	Name   string `json:"name"`
	Amount int64  `json:"amount"` // a year
	Count  int    `json:"count"`
}

type planEquitySale struct {
	AccountID string `json:"account_id"`
	Date      string `json:"date"`
	Name      string `json:"name"`
	Amount    int64  `json:"amount"`
}

var (
	rothName   = regexp.MustCompile(`(?i)roth`)
	pretaxName = regexp.MustCompile(`(?i)401 ?\(?k\)?|403 ?\(?b\)?|457|\bira\b|\bsep\b|\bsimple\b|\btsp\b|pension|\bhsa\b`)
	hsaName    = regexp.MustCompile(`(?i)\bhsa\b`)
	savingName = regexp.MustCompile(`(?i)saving|money market|\bcd\b`)
	// Interest paid into a cash account, the same words investment_activity reads as income.
	interestName = regexp.MustCompile(`(?i)dividend|interest|bank int\b`)
)

// Contributions into investment accounts, by what their names say; these mirror the words the
// investment_activity view lets through as contributions. Moves between the owner's own accounts aren't new
// money; RSU vests and ESPP purchases arrive as $0 share deposits whose value enters with a later share sale.
var (
	internalName = regexp.MustCompile(`(?i)journal|conversion|withdrawal`)
	esppName     = regexp.MustCompile(`(?i)stock purchase`)
	rsuName      = regexp.MustCompile(`(?i)restricted stock`)
	saleName     = regexp.MustCompile(`(?i)sharesale`)
)

// guessPlanAccount fills an account's defaults from its type and name: cash and investment accounts are in,
// everything else (cards, loans, property) out. A cash account that pays interest grows at the rate it paid on
// its average balance, avg.
func guessPlanAccount(ac *planAccount, avg int64) {
	ac.Include = ac.Type == "depository" || ac.Type == "investment"
	ac.Bucket, ac.Growth, ac.PenaltyFreeAge = "cash", 0, 59.5
	switch {
	case ac.Type != "investment":
		if ac.Interest > 0 && avg > 0 {
			ac.Growth = math.Round(float64(ac.Interest)/float64(avg)*200) / 2
		} else if ac.Interest == 0 && savingName.MatchString(ac.Name) {
			ac.Growth = 2
		}
	case rothName.MatchString(ac.Name):
		ac.Bucket, ac.Growth = "roth", 7
	case pretaxName.MatchString(ac.Name):
		ac.Bucket, ac.Growth = "pretax", 7
		if hsaName.MatchString(ac.Name) {
			ac.PenaltyFreeAge = 65
		}
	default:
		ac.Bucket, ac.Growth = "taxable", 7
	}
}

// contribRow is one contribution into an investment account, from investment_activity.
type contribRow struct {
	account, date, name string
	amount              int64
}

// classifyContributions sorts each account's contributions dated from on into deposits, ESPP, RSU, unclear
// and internal, as totals over the window. rows run in date order and may start before from, so a share sale
// early in the window still finds the vest or purchase it came from.
func classifyContributions(rows []contribRow, from string) (sums map[string]*planAccount, unclear []planEquitySale) {
	sums = map[string]*planAccount{}
	// The dates of each account's $0 deposits by kind, over all history.
	deps := map[string]map[string][]string{}
	for _, r := range rows {
		if internalName.MatchString(r.name) {
			continue
		}
		k := ""
		if esppName.MatchString(r.name) {
			k = "espp"
		} else if rsuName.MatchString(r.name) {
			k = "rsu"
		}
		if k != "" {
			if deps[r.account] == nil {
				deps[r.account] = map[string][]string{}
			}
			deps[r.account][k] = append(deps[r.account][k], r.date)
		}
	}
	// In an account with both kinds a sale belongs to a deposit in the week up to it, if only one kind made one.
	// ponytail: 7 days is a guess (selling at vest settles within days); widen it if sales of one grant regularly
	// land later, or drop it once the feed says which lot a sale closed.
	inWeek := func(dates []string, sale string) bool {
		d, err := time.Parse(time.DateOnly, sale)
		lo := d.AddDate(0, 0, -7).Format(time.DateOnly)
		return err == nil && slices.ContainsFunc(dates, func(x string) bool { return x >= lo && x <= sale })
	}
	for _, r := range rows {
		kind := "deposit"
		switch {
		case internalName.MatchString(r.name):
			kind = "internal"
		case esppName.MatchString(r.name):
			kind = "espp"
		case rsuName.MatchString(r.name):
			kind = "rsu"
		case saleName.MatchString(r.name):
			k := deps[r.account]
			e, v := inWeek(k["espp"], r.date), inWeek(k["rsu"], r.date)
			switch {
			case len(k["rsu"]) > 0 && len(k["espp"]) == 0:
				kind = "rsu"
			case len(k["espp"]) > 0 && len(k["rsu"]) == 0:
				kind = "espp"
			case e && !v:
				kind = "espp"
			case v && !e:
				kind = "rsu"
			default:
				kind = "unclear"
			}
		}
		if r.date < from {
			continue
		}
		s := sums[r.account]
		if s == nil {
			s = &planAccount{}
			sums[r.account] = s
		}
		switch kind {
		case "deposit":
			s.Deposits += r.amount
		case "espp":
			s.ESPP += r.amount
		case "rsu":
			s.RSU += r.amount
		case "internal":
			s.Internal += max(r.amount, 0)
		case "unclear":
			s.Unclear += r.amount
			unclear = append(unclear, planEquitySale{AccountID: r.account, Date: r.date, Name: r.name, Amount: r.amount})
		}
	}
	return sums, unclear
}

func (a *app) planBaseline(ctx context.Context, today time.Time) (planBaseline, error) {
	b := planBaseline{Accounts: []planAccount{}, IncomeSources: []planSource{}, UnclearSales: []planEquitySale{}}
	cur := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	var first sql.NullString
	if err := a.db.QueryRowContext(ctx, `SELECT MIN(date) FROM cashflow`).Scan(&first); err != nil {
		return b, err
	}
	if first.Valid {
		f, _ := time.Parse("2006-01", first.String[:7])
		b.Months = min(12, (cur.Year()-f.Year())*12+int(cur.Month()-f.Month()))
	}
	from, to := cur.AddDate(0, -max(b.Months, 0), 0).Format(time.DateOnly), cur.Format(time.DateOnly)
	yearly := func(sum int64) int64 { return sum * 12 / int64(max(b.Months, 1)) }
	var in int64
	bySource := map[string]*planSource{}
	// Interest paid into each account by source: it's left out of income when the account's growth carries it.
	interest := map[string]map[string]*planSource{}
	if b.Months > 0 {
		rows, err := a.db.QueryContext(ctx, `SELECT account_id,amount,COALESCE(NULLIF(merchant,''),name) FROM cashflow WHERE date>=$1 AND date<$2`, from, to)
		if err != nil {
			return b, err
		}
		var out int64
		for rows.Next() {
			var acct, name string
			var amount int64
			if err := rows.Scan(&acct, &amount, &name); err != nil {
				rows.Close()
				return b, err
			}
			switch {
			case amount < 0:
				out -= amount
			case interestName.MatchString(name):
				if interest[acct] == nil {
					interest[acct] = map[string]*planSource{}
				}
				addSource(interest[acct], name, amount)
			default:
				in += amount
				addSource(bySource, name, amount)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return b, err
		}
		b.Spending = out / int64(b.Months)
	}
	// Average balance over the same months, so a drawn-down account's interest isn't read against what is left.
	avgBal := map[string]int64{}
	brows, err := a.db.QueryContext(ctx, `SELECT account_id,AVG(current)::float8 FROM balances WHERE date>=$1 AND date<$2 GROUP BY account_id`, from, to)
	if err != nil {
		return b, err
	}
	for brows.Next() {
		var id string
		var avg float64
		if err := brows.Scan(&id, &avg); err != nil {
			brows.Close()
			return b, err
		}
		avgBal[id] = int64(avg)
	}
	brows.Close()
	if err := brows.Err(); err != nil {
		return b, err
	}
	// Contributions that came from outside the owner's accounts (payroll, ESPP); a matched transfer from checking
	// is already counted as cash-flow surplus. All history is read so a sale can find its vest or purchase.
	var crows []contribRow
	rows, err := a.db.QueryContext(ctx, `SELECT ia.account_id,ia.date,ia.name,ia.amount FROM investment_activity ia JOIN transactions t ON t.id=ia.id
		WHERE ia.kind='contribution' AND t.transfer_id IS NULL AND ia.date<$1 ORDER BY ia.account_id,ia.date,ia.id`, to)
	if err != nil {
		return b, err
	}
	for rows.Next() {
		var r contribRow
		if err := rows.Scan(&r.account, &r.date, &r.name, &r.amount); err != nil {
			rows.Close()
			return b, err
		}
		crows = append(crows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return b, err
	}
	sums, unclear := map[string]*planAccount{}, []planEquitySale(nil)
	if b.Months > 0 {
		sums, unclear = classifyContributions(crows, from)
	}
	b.UnclearSales = append(b.UnclearSales, unclear...)
	rows, err = a.db.QueryContext(ctx, `SELECT id,name,institution,type,COALESCE(current,0) FROM accounts ORDER BY institution,name`)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var ac planAccount
		if err := rows.Scan(&ac.ID, &ac.Name, &ac.Institution, &ac.Type, &ac.Balance); err != nil {
			return b, err
		}
		if s := sums[ac.ID]; s != nil {
			ac.Deposits, ac.ESPP, ac.RSU = max(yearly(s.Deposits), 0), max(yearly(s.ESPP), 0), max(yearly(s.RSU), 0)
			ac.Unclear, ac.Internal = max(yearly(s.Unclear), 0), yearly(s.Internal)
		}
		avg, ok := avgBal[ac.ID]
		if !ok {
			avg = ac.Balance
		}
		var paid int64
		for _, s := range interest[ac.ID] {
			paid += s.Amount
		}
		// ponytail: 6% a year is the ceiling on plausible cash interest; adjust if rates go higher. Above it, or
		// outside a deposit account, the money is a dividend or refund from somewhere else: it stays income.
		if ac.Type == "depository" && avg > 0 && float64(yearly(paid))/float64(avg) <= 0.06 {
			ac.Interest = yearly(paid)
			b.Interest += paid
		} else {
			for name, s := range interest[ac.ID] {
				in += s.Amount
				if bySource[name] == nil {
					bySource[name] = &planSource{Name: name}
				}
				bySource[name].Amount += s.Amount
				bySource[name].Count += s.Count
			}
		}
		guessPlanAccount(&ac, avg)
		ac.Contribution = ac.Deposits + ac.ESPP
		b.Accounts = append(b.Accounts, ac)
	}
	if err := rows.Err(); err != nil {
		return b, err
	}
	if b.Months > 0 {
		b.Income, b.Interest = in/int64(b.Months), b.Interest/int64(b.Months)
	}
	for _, s := range bySource {
		b.IncomeSources = append(b.IncomeSources, planSource{s.Name, yearly(s.Amount), s.Count})
	}
	slices.SortFunc(b.IncomeSources, func(x, y planSource) int {
		return cmp.Or(cmp.Compare(y.Amount, x.Amount), strings.Compare(x.Name, y.Name))
	})
	b.IncomeSources = b.IncomeSources[:min(len(b.IncomeSources), 8)]
	b.Unvested, err = a.unvestedTotal(ctx)
	return b, err
}

func addSource(m map[string]*planSource, name string, amount int64) {
	if m[name] == nil {
		m[name] = &planSource{Name: name}
	}
	m[name].Amount += amount
	m[name].Count++
}

func (a *app) unvestedTotal(ctx context.Context) (int64, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT raw FROM holdings`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return 0, err
		}
		if p, ok := parseHolding(raw); ok && p.Unvested {
			total += p.Value
		}
	}
	return total, rows.Err()
}

// planEvent is a life event. One-time: expense or income in Year. Yearly changes: spending or earning, by
// Amount a year from Year through Until (0 = to the end). Amounts in cents, in today's dollars.
type planEvent struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Year   int    `json:"year"`
	Until  int    `json:"until,omitempty"`
	Amount int64  `json:"amount"`
}

// projAccount is one included account as the projection sees it.
type projAccount struct {
	ID             string
	Bucket         string
	Balance        int64
	Growth         float64 // nominal %
	Contribution   int64   // a year, until retirement
	PenaltyFreeAge float64
}

type planVestsIn struct {
	Total       int64   `json:"total"` // unvested stock, paid out evenly over Years from the start year; what is left at retirement is forfeited
	Years       int     `json:"years"`
	AfterTaxPct float64 `json:"after_tax_pct"`
	Source      string  `json:"source"` // owner, pace (from the RSU sales in the data) or default
}

// planInputs is every number the projection uses, already resolved from the data, defaults and the owner's
// changes. Monthly amounts in cents; rates in %.
type planInputs struct {
	BirthYear, StartYear, RetireAge, EndAge, SSAge int
	Elapsed                                        float64 // share of StartYear already past: in the balances, so not projected again
	Income, Spending, SSMonthly, RetireExtra       int64
	RetireSpendPct, Inflation, ExtraGrowth         float64
	SurplusAccount                                 string // "" = Extra savings
	Accounts                                       []projAccount
	Events                                         []planEvent
	Vests                                          planVestsIn
	SS                                             planSS
}

type planYear struct {
	Year          int   `json:"year"`
	Age           int   `json:"age"`
	Income        int64 `json:"income"`
	Spending      int64 `json:"spending"`
	Contributions int64 `json:"contributions"`
	Withdrawn     int64 `json:"withdrawn"` // before tax and penalty
	Taxes         int64 `json:"taxes"`
	Shortfall     int64 `json:"shortfall"` // spending left unpaid once every account is empty
	Assets        int64 `json:"assets"`    // at the end of the year
}

// Flat tax on a withdrawal by bucket, Monarch's defaults: taxable 15% on 80% of it being gains.
var withdrawTax = map[string]float64{"cash": 0, "taxable": 0.12, "pretax": 0.20, "roth": 0}

const earlyPenalty = 0.10

// project runs the plan year by year in today's dollars (every rate made real by inflation). Each year:
// income and events, then contributions, then the surplus saved or the shortfall withdrawn (extra savings,
// cash, taxable, pre-tax, Roth, grossed up for tax and the early penalty), then growth on the starting
// balance plus half the year's flow. runOut is the first age with a shortfall, 0 if none.
func project(in planInputs) (years []planYear, runOut int) {
	real := func(g float64) float64 { return (1+g/100)/(1+in.Inflation/100) - 1 }
	type pot struct {
		bal, flow      float64
		rate, tax      float64
		penaltyFreeAge float64
		bucket         string
	}
	pots := []*pot{{rate: real(in.ExtraGrowth), bucket: "extra"}} // Extra savings
	surplus := pots[0]
	for _, ac := range in.Accounts {
		p := &pot{bal: float64(ac.Balance), rate: real(ac.Growth), tax: withdrawTax[ac.Bucket], penaltyFreeAge: ac.PenaltyFreeAge, bucket: ac.Bucket}
		pots = append(pots, p)
		if ac.ID == in.SurplusAccount {
			surplus = p
		}
	}
	order := []*pot{pots[0]}
	for _, b := range []string{"cash", "taxable", "pretax", "roth"} {
		for _, p := range pots[1:] {
			if p.bucket == b {
				order = append(order, p)
			}
		}
	}
	vestLeft, vestYear := 0.0, 0.0
	if v := in.Vests; v.Years > 0 {
		vestLeft = float64(v.Total) * v.AfterTaxPct / 100
		vestYear = vestLeft / float64(v.Years)
	}
	for y := in.StartYear; y-in.BirthYear <= in.EndAge; y++ {
		age := y - in.BirthYear
		working := age < in.RetireAge
		// The first year is only what's left of it; one-time events in it still count in full.
		part := 1.0
		if y == in.StartYear {
			part = 1 - in.Elapsed
		}
		share := func(c int64) int64 { return int64(math.Round(float64(c) * part)) }
		py := planYear{Year: y, Age: age}
		if working {
			py.Income = share(in.Income * 12)
		}
		if age >= in.SSAge {
			py.Income += share(in.SSMonthly * 12)
		}
		py.Spending = share(in.Spending * 12)
		if !working {
			py.Spending = share(int64(math.Round(float64(in.Spending*12)*in.RetireSpendPct/100)) + in.RetireExtra)
		}
		for _, e := range in.Events {
			switch yearly := y >= e.Year && (e.Until == 0 || y <= e.Until); {
			case e.Kind == "expense" && y == e.Year:
				py.Spending += e.Amount
			case e.Kind == "income" && y == e.Year:
				py.Income += e.Amount
			case e.Kind == "spending" && yearly:
				py.Spending += share(e.Amount)
			case e.Kind == "earning" && yearly:
				py.Income += share(e.Amount)
			}
		}
		// Vests while working; whatever hasn't vested at retirement is forfeited.
		if working && vestLeft > 0 {
			pay := math.Round(min(vestLeft, vestYear*part))
			py.Income += int64(pay)
			vestLeft -= pay
		}
		for _, p := range pots {
			p.flow = 0
		}
		if working {
			for i, ac := range in.Accounts {
				c := share(ac.Contribution)
				pots[i+1].flow += float64(c)
				py.Contributions += c
			}
		}
		if net := py.Income - py.Spending; net >= 0 {
			surplus.flow += float64(net)
		} else {
			need := float64(-net)
			for _, p := range order {
				t := p.tax
				if (p.bucket == "pretax" || p.bucket == "roth") && float64(age) < p.penaltyFreeAge {
					t += earlyPenalty
				}
				take := min(math.Round(need/(1-t)), p.bal+p.flow)
				if take <= 0 {
					continue
				}
				tax := math.Round(take * t)
				p.flow -= take
				py.Withdrawn += int64(take)
				py.Taxes += int64(tax)
				need = max(0, need-(take-tax))
				if need == 0 {
					break
				}
			}
			py.Shortfall = int64(math.Round(need))
		}
		for _, p := range pots {
			p.bal = max(0, p.bal+p.flow+math.Round(p.rate*part*(p.bal+p.flow/2)))
			py.Assets += int64(p.bal)
		}
		if py.Shortfall > 0 && runOut == 0 {
			runOut = age
		}
		years = append(years, py)
	}
	return years, runOut
}

// planDoc is what the owner has set. A nil field means "from the data, or the default".
type planDoc struct {
	BirthYear      int                       `json:"birth_year"`
	RetireAge      *int                      `json:"retire_age,omitempty"`
	EndAge         *int                      `json:"end_age,omitempty"`
	SSAge          *int                      `json:"ss_age,omitempty"`
	SSMonthly      *int64                    `json:"ss_monthly,omitempty"`
	RetireSpendPct *float64                  `json:"retire_spend_pct,omitempty"`
	RetireExtra    *int64                    `json:"retire_extra,omitempty"` // a year
	Inflation      *float64                  `json:"inflation,omitempty"`
	Income         *int64                    `json:"income_monthly,omitempty"`
	Spending       *int64                    `json:"spending_monthly,omitempty"`
	SurplusAccount string                    `json:"surplus_account,omitempty"`
	Accounts       map[string]planAccountDoc `json:"accounts,omitempty"`
	Events         []planEvent               `json:"events,omitempty"`
	Vests          planVestsDoc              `json:"vests"`
}

type planAccountDoc struct {
	Include      *bool    `json:"include,omitempty"`
	Bucket       *string  `json:"bucket,omitempty"`
	Growth       *float64 `json:"growth,omitempty"`
	Contribution *int64   `json:"contribution,omitempty"`
}

// planVestsDoc: unvested stock paid out as income, by default the Investments page's unvested total over 4
// years at 65% after tax; the total follows the data until the owner sets it.
type planVestsDoc struct {
	Total       *int64   `json:"total,omitempty"`
	Years       *int     `json:"years,omitempty"`
	AfterTaxPct *float64 `json:"after_tax_pct,omitempty"`
	Off         bool     `json:"off,omitempty"`
}

// ssEstimate scales the default (a full 35-year career) to the years worked, through the benefit formula:
// invert it for the career's average monthly earnings, take that share, apply the formula again. The 2025 bend
// points make a short career lose less than a straight scale-down. Cents, in whole dollars.
func ssEstimate(full int64, years int) int64 {
	const b1, b2 = 122600.0, 739100.0 // a month
	pia := func(a float64) float64 {
		return 0.9*math.Min(a, b1) + 0.32*math.Max(0, math.Min(a, b2)-b1) + 0.15*math.Max(0, a-b2)
	}
	p, a := float64(full), 0.0
	switch {
	case p <= 0.9*b1:
		a = p / 0.9
	case p <= pia(b2):
		a = b1 + (p-0.9*b1)/0.32
	default:
		a = b2 + (p-pia(b2))/0.15
	}
	return int64(math.Round(pia(a*float64(min(max(years, 0), 35))/35)/100)) * 100
}

// Defaults, from Monarch's Forecasting, except Social Security at 67 (full retirement age for anyone born after
// 1960). Extra savings grow at a savings account's 2%.
var planDefault = struct {
	RetireAge      int     `json:"retire_age"`
	EndAge         int     `json:"end_age"`
	SSAge          int     `json:"ss_age"`
	SSMonthly      int64   `json:"ss_monthly"`
	RetireSpendPct float64 `json:"retire_spend_pct"`
	RetireExtra    int64   `json:"retire_extra"`
	Inflation      float64 `json:"inflation"`
	ExtraGrowth    float64 `json:"extra_growth"`
	VestYears      int     `json:"vest_years"`
	VestAfterTax   float64 `json:"vest_after_tax_pct"`
}{65, 90, 67, 200000, 100, 650000, 3, 2, 4, 65}

func or[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}

func resolvePlan(d planDoc, b planBaseline, today time.Time) planInputs {
	jan1 := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, today.Location())
	in := planInputs{
		BirthYear: d.BirthYear, StartYear: today.Year(), Elapsed: float64(today.YearDay()-1) / float64(jan1.AddDate(1, 0, 0).Sub(jan1).Hours()/24),
		RetireAge: or(d.RetireAge, planDefault.RetireAge), EndAge: or(d.EndAge, planDefault.EndAge), SSAge: or(d.SSAge, planDefault.SSAge), RetireSpendPct: or(d.RetireSpendPct, planDefault.RetireSpendPct),
		RetireExtra: or(d.RetireExtra, planDefault.RetireExtra), Inflation: or(d.Inflation, planDefault.Inflation), ExtraGrowth: planDefault.ExtraGrowth,
		Income: or(d.Income, b.Income), Spending: or(d.Spending, b.Spending),
		SurplusAccount: d.SurplusAccount, Events: d.Events,
	}
	for _, ac := range b.Accounts {
		o := d.Accounts[ac.ID]
		if !or(o.Include, ac.Include) {
			continue
		}
		bucket := or(o.Bucket, ac.Bucket)
		in.Accounts = append(in.Accounts, projAccount{ID: ac.ID, Bucket: bucket, Balance: max(ac.Balance, 0), Growth: or(o.Growth, ac.Growth),
			Contribution: or(o.Contribution, ac.Contribution), PenaltyFreeAge: ac.PenaltyFreeAge})
	}
	if !d.Vests.Off {
		v := planVestsIn{Total: or(d.Vests.Total, b.Unvested), Years: planDefault.VestYears, AfterTaxPct: or(d.Vests.AfterTaxPct, planDefault.VestAfterTax), Source: "default"}
		// The pace of the RSU sales in the data says how fast the unvested stock vests. Those sales are left out of
		// the accounts' contributions, so this is the one place vests count.
		var pace int64
		for _, ac := range b.Accounts {
			pace += ac.RSU
		}
		if d.Vests.Years != nil {
			v.Years, v.Source = *d.Vests.Years, "owner"
		} else if pace > 0 && v.Total > 0 {
			v.Years, v.Source = min(20, max(1, int(math.Ceil(float64(v.Total)*v.AfterTaxPct/100/float64(pace))))), "pace"
		}
		in.Vests = v
	}
	in.SS = planSS{Source: "estimate", YearsWorked: min(max(in.RetireAge-22, 0), 35)} // 22: assumed start of full-time work
	in.SS.Estimate = ssEstimate(planDefault.SSMonthly, in.SS.YearsWorked)
	in.SS.Monthly = in.SS.Estimate
	if d.SSMonthly != nil {
		in.SS.Monthly, in.SS.Source = *d.SSMonthly, "owner"
	}
	in.SSMonthly = in.SS.Monthly
	return in
}

// dataOnly keeps the owner's birth year, events and stock vests, and drops every number they changed.
func (d planDoc) dataOnly() planDoc {
	return planDoc{BirthYear: d.BirthYear, Events: d.Events, Vests: planVestsDoc{Off: d.Vests.Off}}
}

// validate names the first bad field.
func (d planDoc) validate(year int) error {
	bad := func(field, why string) error { return fmt.Errorf("%s %s", field, why) }
	if d.BirthYear < year-110 || d.BirthYear > year-18 {
		return bad("birth_year", fmt.Sprintf("must be between %d and %d", year-110, year-18))
	}
	for f, p := range map[string]*int{"retire_age": d.RetireAge, "end_age": d.EndAge, "ss_age": d.SSAge} {
		if p != nil && (*p < 18 || *p > 110) {
			return bad(f, "must be an age from 18 to 110")
		}
	}
	if d.EndAge != nil && *d.EndAge <= year-d.BirthYear {
		return bad("end_age", "must be after your current age")
	}
	for f, p := range map[string]*float64{"inflation": d.Inflation} {
		if p != nil && (*p < -10 || *p > 20) {
			return bad(f, "must be from -10% to 20%")
		}
	}
	if p := d.RetireSpendPct; p != nil && (*p < 0 || *p > 300) {
		return bad("retire_spend_pct", "must be from 0% to 300%")
	}
	for f, p := range map[string]*int64{"ss_monthly": d.SSMonthly, "retire_extra": d.RetireExtra, "income_monthly": d.Income, "spending_monthly": d.Spending, "vests.total": d.Vests.Total} {
		if p != nil && *p < 0 {
			return bad(f, "can't be negative")
		}
	}
	if p := d.Vests.Years; p != nil && (*p < 1 || *p > 20) {
		return bad("vests.years", "must be from 1 to 20")
	}
	if p := d.Vests.AfterTaxPct; p != nil && (*p < 0 || *p > 100) {
		return bad("vests.after_tax_pct", "must be from 0% to 100%")
	}
	for id, o := range d.Accounts {
		f := "accounts." + id
		if o.Bucket != nil && !slices.Contains([]string{"cash", "taxable", "pretax", "roth"}, *o.Bucket) {
			return bad(f+".bucket", "must be cash, taxable, pretax or roth")
		}
		if o.Growth != nil && (*o.Growth < -10 || *o.Growth > 20) {
			return bad(f+".growth", "must be from -10% to 20%")
		}
		if o.Contribution != nil && *o.Contribution < 0 {
			return bad(f+".contribution", "can't be negative")
		}
	}
	if len(d.Events) > 100 {
		return bad("events", "can have at most 100 entries")
	}
	for i, e := range d.Events {
		f := fmt.Sprintf("events[%d]", i)
		switch {
		case !slices.Contains([]string{"expense", "income", "spending", "earning"}, e.Kind):
			return bad(f+".kind", "must be expense, income, spending or earning")
		case strings.TrimSpace(e.Name) == "" || len(e.Name) > 80:
			return bad(f+".name", "must be 1 to 80 characters")
		case e.Year < year || e.Year > d.BirthYear+110:
			return bad(f+".year", "must be this year or later")
		case e.Until != 0 && e.Until < e.Year:
			return bad(f+".until", "must be on or after the year")
		case (e.Kind == "expense" || e.Kind == "income") && e.Amount < 0:
			return bad(f+".amount", "can't be negative")
		}
	}
	return nil
}

// planSS is the monthly Social Security the plan uses and where it came from: the owner, or an estimate from years worked.
type planSS struct {
	Monthly     int64  `json:"monthly"`
	Source      string `json:"source"` // owner or estimate
	YearsWorked int    `json:"years_worked"`
	Estimate    int64  `json:"estimate"` // what the estimate is at this retirement age, owner or not
}

type planProjection struct {
	Years   []planYear  `json:"years"`
	RunOut  int         `json:"run_out"` // age, 0 = lasts to the end of the plan
	Vests   planVestsIn `json:"vests"`
	SS      planSS      `json:"ss"`
	Elapsed float64     `json:"elapsed"`
}

func runPlan(d planDoc, b planBaseline, today time.Time) planProjection {
	in := resolvePlan(d, b, today)
	years, runOut := project(in)
	return planProjection{years, runOut, in.Vests, in.SS, in.Elapsed}
}

func (a *app) planRoutes(mux *http.ServeMux) {
	view := func(w http.ResponseWriter, r *http.Request) {
		today := time.Now()
		b, err := a.planBaseline(r.Context(), today)
		if err != nil {
			apiError(w, err)
			return
		}
		out := map[string]any{"doc": nil, "baseline": b, "defaults": planDefault, "projection": nil, "data_projection": nil}
		var raw []byte
		switch err := a.db.QueryRowContext(r.Context(), `SELECT doc FROM plan WHERE id=1`).Scan(&raw); {
		case err == sql.ErrNoRows:
		case err != nil:
			apiError(w, err)
			return
		default:
			var d planDoc
			if err := json.Unmarshal(raw, &d); err != nil {
				apiError(w, err)
				return
			}
			out["doc"], out["projection"], out["data_projection"] = d, runPlan(d, b, today), runPlan(d.dataOnly(), b, today)
		}
		jsonResponse(w, 200, out)
	}
	read := func(w http.ResponseWriter, r *http.Request) (planDoc, bool) {
		var d planDoc
		if !decode(w, r, &d) {
			return d, false
		}
		if err := d.validate(time.Now().Year()); err != nil {
			jsonResponse(w, 400, map[string]string{"error": err.Error()})
			return d, false
		}
		return d, true
	}
	mux.HandleFunc("GET /api/plan", view)
	mux.HandleFunc("PUT /api/plan", func(w http.ResponseWriter, r *http.Request) {
		d, ok := read(w, r)
		if !ok {
			return
		}
		raw, _ := json.Marshal(d)
		if _, err := a.db.ExecContext(r.Context(), `INSERT INTO plan(id,doc) VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET doc=$1,updated_at=now()`, raw); err != nil {
			apiError(w, err)
			return
		}
		view(w, r)
	})
	// A what-if: the projection for a plan that isn't saved, with the data-only one to compare.
	mux.HandleFunc("POST /api/plan/projection", func(w http.ResponseWriter, r *http.Request) {
		d, ok := read(w, r)
		if !ok {
			return
		}
		today := time.Now()
		b, err := a.planBaseline(r.Context(), today)
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"projection": runPlan(d, b, today), "data_projection": runPlan(d.dataOnly(), b, today)})
	})
}

type goalLink struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name,omitempty"`
	Pct       int    `json:"pct"`
}

type goalInput struct {
	Name         string     `json:"name"`
	Target       *int64     `json:"target"`        // cents, or
	TargetMonths *int       `json:"target_months"` // months of the plan's spending
	TargetDate   string     `json:"target_date"`   // YYYY-MM or ""
	Accounts     []goalLink `json:"accounts"`
}

// goalView is a goal with its progress: Current is the linked balances times their percentages; Needed is the
// saving a month to reach Target by TargetDate; Pace is how much Current grew a month over the last 6 months.
type goalView struct {
	ID int64 `json:"id"`
	goalInput
	Target  int64  `json:"target_amount"`
	Current int64  `json:"current"`
	Reached bool   `json:"reached"`
	Needed  *int64 `json:"needed_monthly,omitempty"`
	Pace    *int64 `json:"pace_monthly,omitempty"`
}

var goalMonth = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

func (g goalInput) validate() error {
	switch {
	case strings.TrimSpace(g.Name) == "" || len(g.Name) > 80:
		return errors.New("name must be 1 to 80 characters")
	case (g.Target == nil) == (g.TargetMonths == nil):
		return errors.New("set either a target amount or a number of months of expenses")
	case g.Target != nil && *g.Target <= 0:
		return errors.New("target must be more than zero")
	case g.TargetMonths != nil && (*g.TargetMonths < 1 || *g.TargetMonths > 120):
		return errors.New("target_months must be from 1 to 120")
	case g.TargetDate != "" && !goalMonth.MatchString(g.TargetDate):
		return errors.New("target_date must be YYYY-MM")
	}
	for _, l := range g.Accounts {
		if l.Pct < 1 || l.Pct > 100 {
			return errors.New("each account's share must be from 1% to 100%")
		}
	}
	return nil
}

// planSpending is the monthly spending the plan uses: the owner's figure if they set one, else the data's.
func (a *app) planSpending(ctx context.Context, b planBaseline) (int64, error) {
	var raw []byte
	err := a.db.QueryRowContext(ctx, `SELECT doc FROM plan WHERE id=1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return b.Spending, nil
	}
	var d planDoc
	if err == nil {
		err = json.Unmarshal(raw, &d)
	}
	return or(d.Spending, b.Spending), err
}

func (a *app) goals(ctx context.Context, today time.Time) ([]goalView, error) {
	b, err := a.planBaseline(ctx, today)
	if err != nil {
		return nil, err
	}
	spending, err := a.planSpending(ctx, b)
	if err != nil {
		return nil, err
	}
	balance := map[string]planAccount{}
	for _, ac := range b.Accounts {
		balance[ac.ID] = ac
	}
	sixAgo := today.AddDate(0, -6, 0).Format(time.DateOnly)
	out := []goalView{}
	rows, err := a.db.QueryContext(ctx, `SELECT g.id,g.name,g.target_cents,g.target_months,g.target_date,
		COALESCE(json_agg(json_build_object('account_id',l.account_id,'pct',l.pct)) FILTER (WHERE l.account_id IS NOT NULL),'[]')
		FROM goals g LEFT JOIN goal_accounts l ON l.goal_id=g.id GROUP BY g.id ORDER BY g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g goalView
		var links []byte
		if err := rows.Scan(&g.ID, &g.Name, &g.goalInput.Target, &g.TargetMonths, &g.TargetDate, &links); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(links, &g.Accounts); err != nil {
			return nil, err
		}
		g.Target = or(g.goalInput.Target, spending*int64(or(g.TargetMonths, 0)))
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		g := &out[i]
		var past int64
		for j, l := range g.Accounts {
			ac := balance[l.AccountID]
			g.Accounts[j].Name = ac.Name
			now := max(ac.Balance, 0) * int64(l.Pct) / 100
			g.Current += now
			then := sql.NullInt64{Int64: max(ac.Balance, 0), Valid: true} // no history that far back: no change
			if err := a.db.QueryRowContext(ctx, `SELECT current FROM balances WHERE account_id=$1 AND date<=$2 ORDER BY date DESC LIMIT 1`, l.AccountID, sixAgo).Scan(&then); err != nil && err != sql.ErrNoRows {
				return nil, err
			}
			past += max(then.Int64, 0) * int64(l.Pct) / 100
		}
		g.Reached = g.Current >= g.Target
		pace := (g.Current - past) / 6
		g.Pace = &pace
		if g.TargetDate != "" && !g.Reached {
			due, _ := time.Parse("2006-01", g.TargetDate)
			months := max(1, (due.Year()-today.Year())*12+int(due.Month()-today.Month()))
			need := (g.Target - g.Current + int64(months) - 1) / int64(months)
			g.Needed = &need
		}
	}
	return out, nil
}

func (a *app) goalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/goals", func(w http.ResponseWriter, r *http.Request) {
		v, err := a.goals(r.Context(), time.Now())
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, v)
	})
	// save creates (id 0) or replaces a goal and its links. An account's shares across all goals total at most
	// 100%; the check and the write share one transaction.
	save := func(w http.ResponseWriter, r *http.Request, id int64) {
		var g goalInput
		if !decode(w, r, &g) {
			return
		}
		if err := g.validate(); err != nil {
			jsonResponse(w, 400, map[string]string{"error": err.Error()})
			return
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			apiError(w, err)
			return
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(r.Context(), `LOCK TABLE goal_accounts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			apiError(w, err)
			return
		}
		for _, l := range g.Accounts {
			var used int
			var name string
			if err := tx.QueryRowContext(r.Context(), `SELECT a.name,COALESCE((SELECT SUM(pct) FROM goal_accounts WHERE account_id=a.id AND goal_id<>$2),0) FROM accounts a WHERE a.id=$1`,
				l.AccountID, id).Scan(&name, &used); err == sql.ErrNoRows {
				jsonResponse(w, 400, map[string]string{"error": "account " + l.AccountID + " not found"})
				return
			} else if err != nil {
				apiError(w, err)
				return
			}
			if used+l.Pct > 100 {
				jsonResponse(w, 400, map[string]string{"error": fmt.Sprintf("%s is already %d%% linked to other goals; %d%% is still free.", name, used, 100-used)})
				return
			}
		}
		if id == 0 {
			err = tx.QueryRowContext(r.Context(), `INSERT INTO goals(name,target_cents,target_months,target_date) VALUES($1,$2,$3,$4) RETURNING id`,
				g.Name, g.Target, g.TargetMonths, g.TargetDate).Scan(&id)
		} else {
			var res sql.Result
			if res, err = tx.ExecContext(r.Context(), `UPDATE goals SET name=$2,target_cents=$3,target_months=$4,target_date=$5 WHERE id=$1`, id, g.Name, g.Target, g.TargetMonths, g.TargetDate); err == nil {
				if n, _ := res.RowsAffected(); n == 0 {
					jsonResponse(w, 404, map[string]string{"error": "goal not found"})
					return
				}
				_, err = tx.ExecContext(r.Context(), `DELETE FROM goal_accounts WHERE goal_id=$1`, id)
			}
		}
		for _, l := range g.Accounts {
			if err == nil {
				_, err = tx.ExecContext(r.Context(), `INSERT INTO goal_accounts(goal_id,account_id,pct) VALUES($1,$2,$3)`, id, l.AccountID, l.Pct)
			}
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]int64{"id": id})
	}
	mux.HandleFunc("POST /api/goals", func(w http.ResponseWriter, r *http.Request) { save(w, r, 0) })
	mux.HandleFunc("PUT /api/goals/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonResponse(w, 404, map[string]string{"error": "goal not found"})
			return
		}
		save(w, r, id)
	})
	mux.HandleFunc("DELETE /api/goals/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := a.db.ExecContext(r.Context(), `DELETE FROM goals WHERE id::text=$1`, r.PathValue("id")); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"deleted": true})
	})
}
