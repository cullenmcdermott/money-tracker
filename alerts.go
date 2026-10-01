package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Alerts flag charges worth a look (a stolen card shows up as a new merchant, an odd amount, a test charge, a
// duplicate or a lone purchase far from home) and categories running well above their usual pace. Detection runs
// after each sync over the last 14 days, the window sync re-reads, and stores candidates in alerts.
// ponytail: thresholds are constants, tune them against real alerts.
const (
	alertWindowDays   = 14
	alertMinHistory   = 60    // days of account history before judging new merchants, amounts and card testing
	alertNewMerchant  = 25000 // cents
	alertSpikeTimes   = 3     // a spike is this many times the most the merchant ever charged...
	alertSpikeOver    = 5000  // ...and at least this much more
	alertTestMax      = 500   // a card-testing charge is under this...
	alertTestDays     = 3     // ...followed within this many days by a new-merchant charge of alertNewMerchant or more
	alertDuplicateMin = 2000
	alertAwayDays     = 3     // a charge out of state needs another merchant in that state within this many days
	alertAwayHold     = 2     // days before an away charge is judged, so the rest of a trip can arrive
	alertHomeDays     = 90    // home is the most common state among located charges in this many days
	alertPaceDay      = 7     // first day of the month a pace alert can fire
	alertPaceMin      = 5000  // a category's usual month must be at least this...
	alertPaceOver     = 10000 // ...and spending so far at least this much (and 25%) above the usual by this day
)

type alertTx struct {
	id, account, merchant, name, desc, category string // merchant: resolved key; name: display name; desc: bank description
	date                                        time.Time
	amount                                      int64 // positive cents spent
}

type alertCandidate struct {
	kind, key, tx, related string
	reasons                []string
}

const day = 24 * time.Hour

// detectChargeAlerts judges the charges in the window against all earlier ones. txs are cash-flow outflows sorted
// by date; first is each account's earliest transaction date; bases are merchants whose described location is the
// company's, not the owner's.
func detectChargeAlerts(txs []alertTx, first map[string]time.Time, bases map[string]bool, today time.Time) []alertCandidate {
	var out []alertCandidate
	add := func(kind string, t alertTx, related string, reason string) {
		out = append(out, alertCandidate{kind: kind, key: t.id, tx: t.id, related: related, reasons: []string{reason}})
	}
	from, judgeAway := today.AddDate(0, 0, -alertWindowDays), today.AddDate(0, 0, -alertAwayHold)
	home := homeState(txs, today)
	seen := map[string][]int64{} // merchant -> earlier amounts
	fresh := make([]bool, len(txs))
	for i, t := range txs {
		prior := seen[t.merchant]
		fresh[i] = len(prior) == 0
		seen[t.merchant] = append(prior, t.amount)
		if t.date.Before(from) || t.merchant == "" {
			continue
		}
		seasoned := t.date.Sub(first[t.account]) >= alertMinHistory*day
		if seasoned && fresh[i] && t.amount >= alertNewMerchant {
			add("new_merchant", t, "", "First charge ever from this merchant")
		}
		// Against the largest earlier charge, not the median: a store with occasional big orders isn't unusual.
		if most := slices.Max(append([]int64{0}, prior...)); seasoned && len(prior) >= 3 && t.amount >= alertSpikeTimes*most && t.amount >= most+alertSpikeOver {
			add("unusual_amount", t, "", fmt.Sprintf("About %.0f× the most this merchant charged before (%s)", float64(t.amount)/float64(most), dollars(most)))
		}
		for j := i - 1; j >= 0 && seasoned && fresh[i] && t.amount >= alertNewMerchant && t.date.Sub(txs[j].date) <= alertTestDays*day; j-- {
			if s := txs[j]; s.account == t.account && fresh[j] && s.amount < alertTestMax && s.merchant != t.merchant {
				add("card_testing", t, s.id, fmt.Sprintf("A %s charge at another new merchant came %s before, on the same account", dollars(s.amount), ago(t.date.Sub(s.date))))
				break
			}
		}
		for j := i - 1; j >= 0 && txs[j].date.Equal(t.date); j-- {
			if s := txs[j]; t.amount >= alertDuplicateMin && s.account == t.account && s.merchant == t.merchant && s.amount == t.amount {
				add("duplicate", t, s.id, "Same merchant, amount and account as another charge that day")
				break
			}
		}
		if st := chargeState(t.desc); st != "" && home != "" && st != home && !bases[t.merchant] && !t.date.After(judgeAway) && alone(txs, t, st) {
			add("away", t, "", fmt.Sprintf("Charged in %s, with no other charges there within %d days (home looks like %s)", st, alertAwayDays, home))
		}
	}
	return out
}

// homeState is the most common state among located charges in the last alertHomeDays days.
func homeState(txs []alertTx, today time.Time) string {
	n := map[string]int{}
	best := ""
	for _, t := range txs {
		if st := chargeState(t.desc); st != "" && !t.date.Before(today.AddDate(0, 0, -alertHomeDays)) {
			if n[st]++; n[st] > n[best] {
				best = st
			}
		}
	}
	return best
}

// alone reports whether no other merchant charged in state st within alertAwayDays of t, on any account.
func alone(txs []alertTx, t alertTx, st string) bool {
	for _, o := range txs {
		if o.merchant != t.merchant && (o.date.Sub(t.date)).Abs() <= alertAwayDays*day && chargeState(o.desc) == st {
			return false
		}
	}
	return true
}

// detectPace flags categories where spending so far this month is at least 25% (and alertPaceOver) above the
// average through the same day of the last 3 months. Comparing the same days, not projecting a daily rate, keeps
// bills that post early in the month from looking like overspending. The key holds only month and category, so a
// rising total never reopens a dismissed alert.
func detectPace(txs []alertTx, today time.Time) []alertCandidate {
	if today.Day() < alertPaceDay {
		return nil
	}
	cats := map[string]bool{}
	for _, t := range txs {
		cats[t.category] = t.category != "" && !t.date.Before(firstOfMonth(today))
	}
	var out []alertCandidate
	for cat, ok := range cats {
		if !ok {
			continue
		}
		soFar, byNow, usual := paceStats(txs, cat, today)
		if usual < alertPaceMin || soFar*4 < byNow*5 || soFar-byNow < alertPaceOver {
			continue
		}
		band := "25–50%"
		if soFar*2 > byNow*3 {
			band = "over 50%"
		}
		out = append(out, alertCandidate{kind: "pace", key: today.Format("2006-01") + ":" + cat, reasons: []string{"Spending so far is " + band + " above the usual by this day of the month"}})
	}
	slices.SortFunc(out, func(a, b alertCandidate) int { return strings.Compare(a.key, b.key) })
	return out
}

// paceStats is one category's spending in day's month through day, the 3 months before's average through the same
// day of the month, and their average for the whole month.
func paceStats(txs []alertTx, cat string, day time.Time) (soFar, byNow, usual int64) {
	start := firstOfMonth(day)
	for _, t := range txs {
		if t.category != cat || t.date.Before(start.AddDate(0, -3, 0)) || t.date.After(day) {
			continue
		}
		if !t.date.Before(start) {
			soFar += t.amount
			continue
		}
		usual += t.amount
		if t.date.Day() <= day.Day() {
			byNow += t.amount
		}
	}
	return soFar, byNow / 3, usual / 3
}

func firstOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func dollars(cents int64) string {
	if cents%100 == 0 {
		return fmt.Sprintf("$%d", cents/100)
	}
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}

func ago(d time.Duration) string {
	switch n := int(math.Round(d.Hours() / 24)); n {
	case 0:
		return "the same day"
	case 1:
		return "1 day"
	default:
		return fmt.Sprintf("%d days", n)
	}
}

// detectAlerts runs every detector and inserts new candidates. Existing (kind,key) rows are left alone, so
// re-runs are idempotent and dismissals stick.
func (a *app) detectAlerts(ctx context.Context, today time.Time) error {
	txs, err := a.alertTxs(ctx)
	if err != nil {
		return err
	}
	first := map[string]time.Time{}
	if err := scanEach(ctx, a, `SELECT account_id,MIN(date) FROM transactions GROUP BY account_id`, func(k, v string) error {
		t, err := time.Parse(time.DateOnly, v)
		first[k] = t
		return err
	}); err != nil {
		return err
	}
	home := homeState(txs, today)
	if err := a.jevLocations(ctx, txs, home, today.AddDate(0, 0, -alertWindowDays)); err != nil {
		log.Printf("jev locations: %v", a.jev.redact(err)) // best effort: undecided merchants are judged as purchases
	}
	bases := map[string]bool{}
	if err := scanEach(ctx, a, `SELECT merchant_key,'' FROM merchant_locations WHERE base`, func(k, _ string) error { bases[k] = true; return nil }); err != nil {
		return err
	}
	for _, c := range append(detectChargeAlerts(txs, first, bases, today), detectPace(txs, today)...) {
		reasons, _ := json.Marshal(c.reasons)
		if _, err := a.db.ExecContext(ctx, `INSERT INTO alerts(kind,key,transaction_id,related_transaction_id,reasons) VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),$5) ON CONFLICT(kind,key) DO NOTHING`,
			c.kind, c.key, c.tx, c.related, reasons); err != nil {
			return err
		}
	}
	if err := a.jevTriage(ctx, home); err != nil {
		log.Printf("jev triage: %v", a.jev.redact(err)) // untriaged alerts stay open and are asked about next sync
	}
	return nil
}

var jevLocationCriteria = map[string]string{
	"purchase": "The place is where the purchase happened, e.g. a store, restaurant, hotel or gas station there",
	"base":     "The place is just where the company is based, e.g. an online service, subscription or app billing from its headquarters",
}

var jevTriageCriteria = map[string]string{
	"normal":     "An ordinary charge for this household",
	"unusual":    "Unusual but plausible",
	"suspicious": "Looks like fraud or a billing mistake, worth checking with the bank",
}

// jevLocations asks Jev, once per merchant, whether the out-of-state place in its recent bank descriptions is where
// the purchase happened or where the company is based. Only the display name and up to 3 descriptions are sent.
// Every merchant asked is saved (a weak or missing answer as a purchase), so none is asked twice.
func (a *app) jevLocations(ctx context.Context, txs []alertTx, home string, from time.Time) error {
	if a.jev == nil || home == "" {
		return nil
	}
	known := map[string]bool{}
	if err := scanEach(ctx, a, `SELECT merchant_key,'' FROM merchant_locations`, func(k, _ string) error { known[k] = true; return nil }); err != nil {
		return err
	}
	type merchant struct {
		Name         string   `json:"name"`
		Descriptions []string `json:"descriptions"`
	}
	todo := map[string]*merchant{}
	var keys []string
	for _, t := range txs {
		if st := chargeState(t.desc); t.date.Before(from) || t.merchant == "" || st == "" || st == home || known[t.merchant] {
			continue
		}
		m := todo[t.merchant]
		if m == nil {
			m = &merchant{Name: t.name}
			todo[t.merchant], keys = m, append(keys, t.merchant)
		}
		if len(m.Descriptions) < 3 && !slices.Contains(m.Descriptions, t.desc) {
			m.Descriptions = append(m.Descriptions, t.desc)
		}
	}
	for start := 0; start < len(keys); start += jevBatch {
		batch := keys[start:min(start+jevBatch, len(keys))]
		state, qs := map[string]any{}, map[string]jevQuestion{}
		for i, k := range batch {
			q := "q" + strconv.Itoa(i)
			state[q] = todo[k]
			qs[q] = jevQuestion{"choice", "Merchant " + q + " in the state has bank descriptions ending in a city and state. Is that place where the purchase happened, or just where the company is based?", jevLocationCriteria}
		}
		answers, err := a.jev.choices(ctx, map[string]any{"merchants": state}, qs)
		if err != nil {
			return err
		}
		for i, k := range batch {
			ans := answers["q"+strconv.Itoa(i)]
			if _, err := a.db.ExecContext(ctx, `INSERT INTO merchant_locations(merchant_key,base,source) VALUES($1,$2,'jev') ON CONFLICT DO NOTHING`,
				k, ans.Choice == "base" && ans.Confidence >= a.jev.minConfidence); err != nil {
				return err
			}
		}
	}
	return nil
}

// jevTriage asks Jev to classify open charge alerts it hasn't judged yet. Sent per alert: the bank description,
// amount, reasons, whether the account is a credit card or bank account, and the categories that account is usually
// charged for; plus the home state. Never account names, balances, dates, ids or other transactions.
// ponytail: one batch per sync; a backlog drains over later syncs.
func (a *app) jevTriage(ctx context.Context, home string) error {
	if a.jev == nil {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT al.id,t.name,-t.amount,al.reasons,acc.type,t.account_id FROM alerts al
		JOIN transactions t ON t.id=al.transaction_id JOIN accounts acc ON acc.id=t.account_id
		WHERE al.jev_choice='' AND al.dismissed_at IS NULL ORDER BY al.id LIMIT $1`, jevBatch)
	if err != nil {
		return err
	}
	defer rows.Close()
	type charge struct {
		Description string   `json:"description"`
		Amount      string   `json:"amount"`
		Reasons     []string `json:"reasons"`
		Account     string   `json:"account"`
		Usual       []string `json:"usual_categories"`
	}
	charges, qs, ids, accounts := map[string]*charge{}, map[string]jevQuestion{}, map[string]int64{}, map[string]string{}
	for rows.Next() {
		var id, cents int64
		var c charge
		var reasons []byte
		var typ, account string
		if err := rows.Scan(&id, &c.Description, &cents, &reasons, &typ, &account); err != nil {
			return err
		}
		json.Unmarshal(reasons, &c.Reasons)
		c.Amount, c.Account = dollars(cents), "bank account"
		if typ == "credit" {
			c.Account = "credit card"
		}
		q := "q" + strconv.Itoa(len(ids))
		charges[q], ids[q], accounts[q] = &c, id, account
		qs[q] = jevQuestion{"choice", "A fraud check flagged charge " + q + " in the state for the listed reasons. How does it look?", jevTriageCriteria}
	}
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return err
	}
	rows.Close()
	for q, account := range accounts {
		if err := scanEach(ctx, a, `SELECT effective_category,'' FROM cashflow WHERE account_id=$1 AND amount<0 AND effective_category<>'' AND date>=to_char(now()-interval '90 days','YYYY-MM-DD')
			GROUP BY 1 ORDER BY COUNT(*) DESC LIMIT 5`, func(k, _ string) error { charges[q].Usual = append(charges[q].Usual, k); return nil }, account); err != nil {
			return err
		}
	}
	household := map[string]string{"home_state": home}
	if a.jev.home != "" {
		household["home"] = a.jev.home
	}
	answers, err := a.jev.choices(ctx, map[string]any{"household": household, "charges": charges}, qs)
	if err != nil {
		return err
	}
	for q, id := range ids {
		ans := answers[q]
		if ans.Choice == "" {
			ans.Choice = "none" // no answer: shown untriaged, and not asked again
		}
		if _, err := a.db.ExecContext(ctx, `UPDATE alerts SET jev_choice=$1,jev_confidence=$2 WHERE id=$3`, ans.Choice, ans.Confidence, id); err != nil {
			return err
		}
	}
	return nil
}

// alertTxs is every cash-flow outflow, oldest first.
func (a *app) alertTxs(ctx context.Context) ([]alertTx, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT c.id,c.account_id,COALESCE(mr.root,c.merchant_key),COALESCE(mr.display_name,NULLIF(c.merchant,''),c.name),c.name,c.date,-c.amount,c.effective_category
		FROM cashflow c LEFT JOIN merchant_roots mr ON mr.key=c.merchant_key WHERE c.amount<0 ORDER BY c.date,c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var txs []alertTx
	for rows.Next() {
		var t alertTx
		var d string
		if err := rows.Scan(&t.id, &t.account, &t.merchant, &t.name, &t.desc, &d, &t.amount, &t.category); err != nil {
			return nil, err
		}
		if t.date, err = time.Parse(time.DateOnly, d); err != nil {
			return nil, err
		}
		txs = append(txs, t)
	}
	return txs, rows.Err()
}

// scanEach runs a two-column text query and calls fn per row.
func scanEach(ctx context.Context, a *app, query string, fn func(k, v string) error, args ...any) error {
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		if err := fn(k, v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// alertsAfterSync is best effort: failures are logged and never fail the sync.
func (a *app) alertsAfterSync(ctx context.Context) {
	today, _ := time.Parse(time.DateOnly, time.Now().Format(time.DateOnly))
	if err := a.detectAlerts(ctx, today); err != nil {
		log.Printf("alerts: %v", err)
	}
}

type alertTxView struct {
	ID          string `json:"id"`
	Date        string `json:"date"`
	Merchant    string `json:"merchant"`
	Description string `json:"description"`
	Amount      int64  `json:"amount"` // cents spent
	Account     string `json:"account"`
}

type alertView struct {
	ID            int64        `json:"id"`
	Kind          string       `json:"kind"`
	Reasons       []string     `json:"reasons"`
	Jev           string       `json:"jev"` // '', none (no answer), normal, unusual or suspicious
	JevConfidence float64      `json:"jev_confidence"`
	Checked       bool         `json:"checked"` // Jev judged it normal, confidently: listed apart from open alerts
	Tx            *alertTxView `json:"tx,omitempty"`
	Related       *alertTxView `json:"related,omitempty"`
	// Pace alerts, computed on each load: the category and month, spending so far (the month's total once it's
	// over), the usual through the same day, and the usual whole month.
	Category   string `json:"category,omitempty"`
	Month      string `json:"month,omitempty"`
	SoFar      int64  `json:"so_far,omitempty"`
	UsualByNow int64  `json:"usual_by_now,omitempty"`
	Usual      int64  `json:"usual,omitempty"`
}

// openAlerts lists alerts not yet marked fine, newest first.
func (a *app) openAlerts(ctx context.Context, today time.Time) ([]alertView, error) {
	minConfidence := 0.5
	if a.jev != nil {
		minConfidence = a.jev.minConfidence
	}
	txCols := func(t, acc string) string {
		return t + `.id,` + t + `.date,COALESCE(mr_` + t + `.display_name,NULLIF(` + t + `.merchant,''),` + t + `.name),` + t + `.name,-` + t + `.amount,` + acc + `.name`
	}
	rows, err := a.db.QueryContext(ctx, `SELECT al.id,al.kind,al.key,al.reasons,al.jev_choice,al.jev_confidence,
		`+txCols("t", "ta")+`,`+txCols("r", "ra")+`
		FROM alerts al LEFT JOIN transactions t ON t.id=al.transaction_id LEFT JOIN accounts ta ON ta.id=t.account_id LEFT JOIN merchant_roots mr_t ON mr_t.key=t.merchant_key
		LEFT JOIN transactions r ON r.id=al.related_transaction_id LEFT JOIN accounts ra ON ra.id=r.account_id LEFT JOIN merchant_roots mr_r ON mr_r.key=r.merchant_key
		WHERE al.dismissed_at IS NULL ORDER BY COALESCE(t.date,'9999') DESC,al.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []alertView{}
	for rows.Next() {
		var v alertView
		var key string
		var reasons []byte
		var t, r [6]sql.NullString
		dest := []any{&v.ID, &v.Kind, &key, &reasons, &v.Jev, &v.JevConfidence}
		for i := range t {
			dest = append(dest, &t[i])
		}
		for i := range r {
			dest = append(dest, &r[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		json.Unmarshal(reasons, &v.Reasons)
		v.Checked = v.Jev == "normal" && v.JevConfidence >= minConfidence
		v.Tx, v.Related = txView(t), txView(r)
		if v.Kind == "pace" {
			v.Month, v.Category, _ = strings.Cut(key, ":")
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var txs []alertTx
	for i, v := range out {
		if v.Kind != "pace" {
			continue
		}
		if txs == nil {
			if txs, err = a.alertTxs(ctx); err != nil {
				return nil, err
			}
		}
		month, _ := time.Parse("2006-01", v.Month)
		day := month.AddDate(0, 1, -1) // a past month's alert shows the whole month
		if today.Before(day) {
			day = today
		}
		out[i].SoFar, out[i].UsualByNow, out[i].Usual = paceStats(txs, v.Category, day)
	}
	return out, nil
}

func txView(c [6]sql.NullString) *alertTxView {
	if !c[0].Valid {
		return nil
	}
	cents, _ := strconv.ParseInt(c[4].String, 10, 64)
	return &alertTxView{ID: c[0].String, Date: c[1].String, Merchant: c[2].String, Description: c[3].String, Amount: cents, Account: c[5].String}
}

func (a *app) alertRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/alerts", func(w http.ResponseWriter, r *http.Request) {
		today, _ := time.Parse(time.DateOnly, time.Now().Format(time.DateOnly))
		v, err := a.openAlerts(r.Context(), today)
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, v)
	})
	// Marking an away-from-home alert fine also records that the merchant's described place is not where the owner was.
	mux.HandleFunc("POST /api/alerts/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		var kind, merchant string
		err := a.db.QueryRowContext(r.Context(), `UPDATE alerts SET dismissed_at=COALESCE(dismissed_at,now()) WHERE id::text=$1
			RETURNING kind,COALESCE((SELECT COALESCE(mr.root,t.merchant_key) FROM transactions t LEFT JOIN merchant_roots mr ON mr.key=t.merchant_key WHERE t.id=transaction_id),'')`,
			r.PathValue("id")).Scan(&kind, &merchant)
		if err == sql.ErrNoRows {
			jsonResponse(w, 404, map[string]string{"error": "alert not found"})
			return
		}
		if err == nil && kind == "away" && merchant != "" {
			_, err = a.db.ExecContext(r.Context(), `INSERT INTO merchant_locations(merchant_key,base,source) VALUES($1,true,'owner')
				ON CONFLICT(merchant_key) DO UPDATE SET base=true,source='owner'`, merchant)
		}
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"dismissed": true})
	})
	// Undo: reopen the alert and drop a base the owner set (Jev is asked again on the next sync).
	mux.HandleFunc("DELETE /api/alerts/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		_, err := a.db.ExecContext(r.Context(), `WITH al AS (UPDATE alerts SET dismissed_at=NULL WHERE id::text=$1 RETURNING kind,transaction_id)
			DELETE FROM merchant_locations ml USING al,transactions t LEFT JOIN merchant_roots mr ON mr.key=t.merchant_key
			WHERE al.kind='away' AND t.id=al.transaction_id AND ml.merchant_key=COALESCE(mr.root,t.merchant_key) AND ml.source='owner'`, r.PathValue("id"))
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"dismissed": false})
	})
}
