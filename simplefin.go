package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errInvalidSetupToken = errors.New("invalid setup token")

// simplefinItemID is the single items row for the SimpleFIN connection (the access URL never goes in it). The access URL lives only in the
// environment, never in the database.
const simplefinItemID = "simplefin"

// simplefinClient talks to SimpleFIN Bridge. accessURL is the long-lived https://user:pass@host/path credential
// from SIMPLEFIN_ACCESS_URL; it must never be logged or stored.
type simplefinClient struct {
	client    *http.Client
	accessURL string
	progress  *syncProgress // nil in tests that don't watch it
}

// syncProgress is the running sync's current step, polled by the Settings page. Its methods are no-ops on nil.
type syncProgress struct {
	mu    sync.Mutex
	state syncState
}

type syncState struct {
	Running   bool      `json:"running"`
	Stage     string    `json:"stage"`
	Done      int       `json:"done"`
	Total     int       `json:"total"` // 0 when the step has no count
	StartedAt time.Time `json:"started_at"`
}

func (p *syncProgress) set(stage string, done, total int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.state.Running {
		p.state = syncState{Running: true, StartedAt: time.Now().UTC()}
	}
	p.state.Stage, p.state.Done, p.state.Total = stage, done, total
}

func (p *syncProgress) finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = syncState{}
}

func (p *syncProgress) get() syncState {
	if p == nil {
		return syncState{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// simplefinFromEnv validates SIMPLEFIN_ACCESS_URL. Empty means not configured. Errors never include the value,
// since url.Parse errors quote it.
func simplefinFromEnv(raw string) (simplefinClient, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return simplefinClient{}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return simplefinClient{}, errors.New("SIMPLEFIN_ACCESS_URL is not a valid URL (expected https://user:pass@host/path)")
	}
	if u.Scheme != "https" || u.Host == "" {
		return simplefinClient{}, errors.New("SIMPLEFIN_ACCESS_URL must be an https URL")
	}
	if _, ok := u.User.Password(); !ok {
		return simplefinClient{}, errors.New("SIMPLEFIN_ACCESS_URL must include credentials (https://user:pass@host/path)")
	}
	return simplefinClient{accessURL: raw, progress: &syncProgress{}}, nil
}

// ensureSimplefinItem upserts the connection row that accounts and sync status hang off.
func (a *app) ensureSimplefinItem() error {
	_, err := a.db.Exec(`INSERT INTO items(id) VALUES($1) ON CONFLICT(id) DO NOTHING`, simplefinItemID)
	return err
}

// syncSimplefin syncs from the env access URL and records last_synced_at / last_error on the connection row.
func (a *app) syncSimplefin(ctx context.Context) error {
	defer a.simplefin.progress.finish()
	warning, err := a.simplefin.syncItem(ctx, a.db, simplefinItemID, a.simplefin.accessURL)
	if err != nil {
		err = errors.New(a.simplefin.redact(err.Error()))
		_, statusErr := a.db.ExecContext(ctx, `UPDATE items SET last_error=$1 WHERE id=$2`, err.Error(), simplefinItemID)
		return errors.Join(err, statusErr)
	}
	_, err = a.db.ExecContext(ctx, `UPDATE items SET last_synced_at=$1,last_error=$2 WHERE id=$3`, time.Now().UTC().Format(time.RFC3339), a.simplefin.redact(warning), simplefinItemID)
	if err == nil {
		if a.jev != nil {
			a.simplefin.progress.set("Suggesting account types", 0, 0)
		}
		a.jevAccountTypes(ctx) // best effort: failures are logged and never fail the sync
		a.simplefin.progress.set("Checking for charges worth a look", 0, 0)
		a.alertsAfterSync(ctx) // best effort too
	}
	return err
}

// redact strips the access URL and its credentials from text before it is stored or returned.
func (s simplefinClient) redact(text string) string {
	u, err := url.Parse(s.accessURL)
	if s.accessURL == "" || err != nil || u.User == nil {
		return text
	}
	text = strings.ReplaceAll(text, s.accessURL, "[redacted]")
	if password, _ := u.User.Password(); password != "" {
		text = strings.ReplaceAll(text, password, "[redacted]")
	}
	return text
}

// runSimplefinClaim redeems a setup token (read from stdin, so it stays out of shell history and ps) once and
// prints only the access URL to stdout. Everything else goes to stderr.
func runSimplefinClaim(stdin io.Reader, stdout, stderr io.Writer, s simplefinClient) int {
	token, err := io.ReadAll(io.LimitReader(stdin, 4096))
	if err != nil {
		fmt.Fprintln(stderr, "error: could not read setup token from stdin")
		return 1
	}
	fmt.Fprintln(stderr, "Claiming setup token (it can only be claimed once)...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	accessURL, err := s.claim(ctx, string(token))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, accessURL)
	fmt.Fprintln(stderr, "Store this access URL in 1Password and expose it as SIMPLEFIN_ACCESS_URL.")
	return 0
}

func (s simplefinClient) httpClient() *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}
	if s.client != nil {
		copy := *s.client
		client = &copy
	}
	checkRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("SimpleFIN redirect must use HTTPS")
		}
		if checkRedirect != nil {
			return checkRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return client
}

func (s simplefinClient) claim(ctx context.Context, token string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(token))
	}
	if err != nil {
		return "", errInvalidSetupToken
	}
	claimURL, err := url.Parse(string(decoded))
	if err != nil || claimURL.Scheme != "https" || claimURL.Host == "" || claimURL.User != nil {
		return "", errInvalidSetupToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claimURL.String(), nil)
	if err != nil {
		return "", errInvalidSetupToken
	}
	res, err := s.httpClient().Do(req)
	if err != nil {
		return "", errors.New("SimpleFIN claim request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusForbidden {
			return "", errors.New("SimpleFIN setup token was rejected; disable it if someone else may have claimed it")
		}
		return "", fmt.Errorf("SimpleFIN claim returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4096))
	if err != nil {
		return "", errors.New("SimpleFIN claim response could not be read")
	}
	accessURL := strings.TrimSpace(string(body))
	u, err := url.Parse(accessURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User == nil {
		return "", errors.New("SimpleFIN claim returned an invalid access URL")
	}
	if _, ok := u.User.Password(); !ok {
		return "", errors.New("SimpleFIN claim returned an invalid access URL")
	}
	return accessURL, nil
}

func decimalCents(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("invalid amount %q", value)
	}
	sign := ""
	if value[0] == '-' || value[0] == '+' {
		sign, value = value[:1], value[1:]
	}
	whole, fraction, hasDot := strings.Cut(value, ".")
	if whole == "" || (hasDot && fraction == "") {
		return 0, errors.New("invalid decimal amount")
	}
	for _, c := range whole + fraction {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid decimal amount")
		}
	}
	fraction = strings.TrimRight(fraction, "0")
	if len(fraction) > 2 {
		return 0, errors.New("amount has more than two decimal places")
	}
	for len(fraction) < 2 {
		fraction += "0"
	}
	cents, err := strconv.ParseInt(sign+whole+fraction, 10, 64)
	if err != nil {
		return 0, errors.New("amount is out of range")
	}
	return cents, nil
}

type simplefinTransaction struct {
	ID           string `json:"id"`
	Posted       int64  `json:"posted"`
	TransactedAt int64  `json:"transacted_at"`
	Amount       string `json:"amount"`
	Description  string `json:"description"`
	Payee        string `json:"payee"`
	Pending      bool   `json:"pending"`
}

type simplefinAccount struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ConnectionID string `json:"conn_id"`
	Org          struct {
		Name string `json:"name"`
	} `json:"org"`
	Currency         string                 `json:"currency"`
	Balance          string                 `json:"balance"`
	AvailableBalance *string                `json:"available-balance"`
	Transactions     []simplefinTransaction `json:"transactions"`
	Holdings         []json.RawMessage      `json:"holdings"` // not in the protocol, but Bridge sends it for some brokerages
}

type simplefinAccountSet struct {
	Errors    []string `json:"errors"`
	ErrorList []struct {
		Message string `json:"msg"`
	} `json:"errlist"`
	Connections []struct {
		ID      string `json:"conn_id"`
		Name    string `json:"name"`
		OrgName string `json:"org_name"`
	} `json:"connections"`
	Accounts []simplefinAccount `json:"accounts"`
}

// simplefinDailyRequests is SimpleFIN Bridge's expected ceiling; going past it ends in a disabled access token.
const simplefinDailyRequests = 24

// spendSimplefinRequest records one request against the rolling 24-hour budget, or refuses once it is spent.
// Failed requests count too: Bridge sees them all.
func spendSimplefinRequest(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM simplefin_requests WHERE at <= now() - interval '24 hours'`); err != nil {
		return err
	}
	var used int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM simplefin_requests`).Scan(&used); err != nil {
		return err
	}
	if used >= simplefinDailyRequests {
		return fmt.Errorf("skipped sync: SimpleFIN's limit of %d requests per 24 hours is used up; try again later", simplefinDailyRequests)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO simplefin_requests DEFAULT VALUES`)
	return err
}

func (s simplefinClient) syncItem(ctx context.Context, db *sql.DB, itemID, accessURL string) (string, error) {
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	end := now.Add(24 * time.Hour)
	floor := now.Add(-89 * 24 * time.Hour)
	// After the first sync, only re-read 14 days before the last one (pending charges post late); accounts we have
	// never seen get the rest of their history in a second pass below.
	start := floor
	var lastSynced sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT last_synced_at FROM items WHERE id=$1`, itemID).Scan(&lastSynced); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if lastSynced.Valid {
		last, err := time.Parse(time.RFC3339, lastSynced.String)
		if err != nil {
			return "", fmt.Errorf("invalid last sync time: %w", err)
		}
		if since := last.Add(-14 * 24 * time.Hour); since.After(start) {
			start = since
		}
	}
	windowDate := start.Format("2006-01-02")
	u, err := url.Parse(accessURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User == nil {
		return "", errors.New("invalid SimpleFIN access URL")
	}
	password, ok := u.User.Password()
	if !ok {
		return "", errors.New("invalid SimpleFIN access URL")
	}
	username := u.User.Username()
	u.User = nil
	u.Path = strings.TrimRight(u.Path, "/") + "/accounts"
	u.RawPath = ""
	var data simplefinAccountSet
	accounts := make(map[string]simplefinAccount)
	warnings := make([]string, 0)
	seenWarnings := make(map[string]bool)
	addWarning := func(message string) {
		if message != "" && !seenWarnings[message] {
			warnings = append(warnings, message)
			seenWarnings[message] = true
		}
	}
	// fetch reads [from, to) in 45-day windows (Bridge warns past 45 days per request), limited to ids when given,
	// and appends the transactions to accounts.
	fetch := func(stage string, from, to time.Time, ids []string) error {
		for from.Before(to) {
			windowEnd := from.Add(45 * 24 * time.Hour)
			if windowEnd.After(to) {
				windowEnd = to
			}
			query := url.Values{"account": ids}
			query.Set("start-date", strconv.FormatInt(from.Unix(), 10))
			query.Set("end-date", strconv.FormatInt(windowEnd.Unix(), 10))
			query.Set("pending", "1")
			query.Set("version", "2")
			u.RawQuery = query.Encode()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
			if err != nil {
				return errors.New("invalid SimpleFIN request URL")
			}
			req.SetBasicAuth(username, password)
			if err := spendSimplefinRequest(ctx, db); err != nil {
				return err
			}
			s.progress.set(stage, 0, 0)
			res, err := s.httpClient().Do(req)
			if err != nil {
				return errors.New("SimpleFIN accounts request failed")
			}
			if res.StatusCode != http.StatusOK {
				res.Body.Close()
				return fmt.Errorf("SimpleFIN accounts returned HTTP %d", res.StatusCode)
			}
			var window simplefinAccountSet
			err = json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&window)
			res.Body.Close()
			if err != nil {
				return fmt.Errorf("invalid SimpleFIN accounts response: %w", err)
			}
			for _, message := range window.Errors {
				addWarning(message)
			}
			for _, item := range window.ErrorList {
				addWarning(item.Message)
			}
			for _, account := range window.Accounts {
				if prev, ok := accounts[account.ID]; ok && ids != nil {
					prev.Transactions = append(prev.Transactions, account.Transactions...) // backfill: keep the current balance
					accounts[account.ID] = prev
					continue
				}
				account.Transactions = append(accounts[account.ID].Transactions, account.Transactions...)
				accounts[account.ID] = account
			}
			if ids == nil {
				data.Connections = window.Connections
			}
			from = windowEnd
		}
		return nil
	}
	if err := fetch("Asking SimpleFIN for accounts", start, end, nil); err != nil {
		return "", err
	}
	if start.After(floor) {
		var newIDs []string
		for id := range accounts {
			var known bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, "sf:"+id).Scan(&known); err != nil {
				return "", err
			}
			if !known {
				newIDs = append(newIDs, id)
			}
		}
		if len(newIDs) > 0 {
			sort.Strings(newIDs)
			if err := fetch(fmt.Sprintf("Fetching history for %d new account(s)", len(newIDs)), floor, start, newIDs); err != nil {
				return "", err
			}
		}
	}
	connections := make(map[string]string, len(data.Connections))
	for _, c := range data.Connections {
		name := c.OrgName
		if name == "" {
			name = c.Name
		}
		connections[c.ID] = name
	}
	dbtx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer dbtx.Rollback()
	done := 0
	for _, account := range accounts {
		s.progress.set("Saving accounts", done, len(accounts))
		done++
		current, err := decimalCents(account.Balance)
		var balance any
		if err != nil {
			addWarning(fmt.Sprintf("skipped balance %s: %v", account.ID, err))
		} else {
			balance = current
		}
		var available any
		if balance != nil && account.AvailableBalance != nil {
			available, err = decimalCents(*account.AvailableBalance)
			if err != nil {
				addWarning(fmt.Sprintf("skipped available balance %s: %v", account.ID, err))
				balance = nil
				available = nil
			}
		}
		institution := account.Org.Name
		if institution == "" {
			institution = connections[account.ConnectionID]
		}
		currency := account.Currency
		if currency == "" {
			currency = "USD"
		}
		accountID := "sf:" + account.ID
		var cents int64
		if balance != nil {
			cents = current
		}
		guess, confident := guessAccountType(institution, account.Name, cents)
		_, err = dbtx.ExecContext(ctx, `INSERT INTO accounts(id,item_id,institution,name,current,available,currency,guessed_type,guess_confident) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT(id) DO UPDATE SET institution=COALESCE(NULLIF(excluded.institution,''),accounts.institution),name=excluded.name,current=excluded.current,available=excluded.available,currency=excluded.currency,guessed_type=`+guessedTypeSQL("excluded.name", "excluded.guessed_type", "excluded.guess_confident")+`,guess_confident=excluded.guess_confident OR `+jevKept("excluded.name", "excluded.guess_confident"),
			accountID, itemID, institution, account.Name, balance, available, currency, guess, confident)
		if err != nil {
			return "", err
		}
		// Holdings are a snapshot: the latest sync replaces the account's rows.
		if _, err := dbtx.ExecContext(ctx, `DELETE FROM holdings WHERE account_id=$1`, accountID); err != nil {
			return "", err
		}
		for _, h := range account.Holdings {
			if _, err := dbtx.ExecContext(ctx, `INSERT INTO holdings(account_id,raw) VALUES($1,$2)`, accountID, string(h)); err != nil {
				return "", err
			}
		}
		if balance != nil {
			if _, err := dbtx.ExecContext(ctx, `INSERT INTO balances(account_id,date,current) VALUES($1,$2,$3) ON CONFLICT(account_id,date) DO UPDATE SET current=excluded.current`, accountID, today.Format("2006-01-02"), current); err != nil {
				return "", err
			}
		} else if _, err := dbtx.ExecContext(ctx, `DELETE FROM balances WHERE account_id=$1 AND date=$2`, accountID, today.Format("2006-01-02")); err != nil {
			return "", err
		}
		if _, err := dbtx.ExecContext(ctx, `DELETE FROM transactions WHERE account_id=$1 AND pending AND date>=$2`, accountID, windowDate); err != nil {
			return "", err
		}
		for _, transaction := range account.Transactions {
			amount, err := decimalCents(transaction.Amount)
			if err != nil {
				addWarning(fmt.Sprintf("skipped transaction %s/%s: %v", account.ID, transaction.ID, err))
				continue
			}
			posted := transaction.Posted
			if posted == 0 {
				posted = transaction.TransactedAt
			}
			if posted == 0 {
				addWarning(fmt.Sprintf("skipped transaction %s/%s: no date", account.ID, transaction.ID))
				continue
			}
			date := time.Unix(posted, 0).UTC().Format("2006-01-02")
			_, err = dbtx.ExecContext(ctx, `INSERT INTO transactions(id,account_id,date,amount,name,merchant,pending) VALUES($1,$2,$3,$4,$5,$6,$7)
				ON CONFLICT(id) DO UPDATE SET account_id=excluded.account_id,date=excluded.date,amount=excluded.amount,name=excluded.name,merchant=excluded.merchant,pending=excluded.pending`,
				"sf:"+account.ID+":"+transaction.ID, accountID, date, amount, transaction.Description, transaction.Payee, transaction.Pending || transaction.Posted == 0)
			if err != nil {
				return "", err
			}
		}
	}
	s.progress.set("Matching transfers and applying rules", 0, 0)
	if err := errors.Join(matchTransfers(ctx, dbtx), assignMerchantKeys(ctx, dbtx), applyRules(ctx, dbtx)); err != nil {
		return "", err
	}
	if err := dbtx.Commit(); err != nil {
		return "", err
	}
	return strings.Join(warnings, "; "), nil
}
