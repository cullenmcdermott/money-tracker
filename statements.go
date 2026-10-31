package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Statement import: transactions and closing balances read from banks' PDF statements, for history older than
// SimpleFIN's and Monarch's.
//
//	money-tracker import-statements [--account <account id or last digits>] [--dry-run] statement.pdf...
//
// Text comes from poppler's `pdftotext -layout` ($PDFTOTEXT, else on PATH), and each layout has a parser below.
// A bank or card statement is imported only when its transactions add up from its opening to its closing
// balance, so a misread line skips the statement instead of saving wrong numbers. Investment statements can't
// be checked that way (prices move), so their activity and closing value are taken as read. --account names
// the account for every file, for cards whose number changed over the years; otherwise the statement's last
// 4 digits are matched like Monarch's. Rows are keyed "stmt:<hash>" and land only before an account's other
// history (SimpleFIN or Monarch), skipping any that history already has (same amount within 3 days).

type stmtTx struct {
	Date   string // YYYY-MM-DD
	Amount int64  // cents, positive = money in
	Name   string
}

// stmtAccount is one account's part of a statement. Balances use accounts.current's sign: owed is negative.
type stmtAccount struct {
	Last4            string
	End              string // closing date, YYYY-MM-DD
	Opening, Closing int64
	Checked          bool // Opening plus the transactions must come to Closing
	Txs              []stmtTx
}

func parseStatement(text string) ([]stmtAccount, error) { // ponytail: a slice for statements covering several accounts; none do yet
	var a stmtAccount
	var err error
	switch {
	case strings.Contains(text, "Opening/Closing Date") && strings.Contains(text, "ACCOUNT ACTIVITY"):
		a, err = parseChaseCard(text)
	case strings.Contains(text, "Statement Closing Date") && strings.Contains(text, "USAA"):
		a, err = parseUSAACard(text)
	case strings.Contains(text, "Statement Period:") && strings.Contains(text, "USAA"):
		a, err = parseUSAABank(text)
	case strings.Contains(text, "DATE..........AMOUNT"):
		a, err = parseUSAAOld(text)
	case strings.Contains(text, "Vanguard") && strings.Contains(text, "Statement overview"):
		a, err = parseVanguard(text)
	default:
		return nil, errors.New("unrecognized statement layout")
	}
	return []stmtAccount{a}, err
}

// money reads "$1,234.56", "-1,234.56", "1,234.56-", "(1,234.56)" or ".21" as cents.
func money(s string) (int64, error) {
	s = strings.NewReplacer("$", "", ",", "").Replace(strings.TrimSpace(s))
	neg := strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.HasPrefix(s, "(")
	s = strings.Trim(s, "-()")
	if strings.HasPrefix(s, ".") {
		s = "0" + s
	}
	c, err := decimalCents(s)
	if neg {
		c = -c
	}
	return c, err
}

func find(pattern, text string) string {
	if m := regexp.MustCompile(pattern).FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// usDate reads MM/DD/YY or MM/DD/YYYY.
func usDate(s string) (time.Time, error) {
	if len(s) == 8 {
		return time.Parse("01/02/06", s)
	}
	return time.Parse("01/02/2006", s)
}

// inYear dates an MM/DD from a statement closing on end: months after end's are from the year before.
func inYear(mmdd string, end time.Time) string {
	var m, d int
	fmt.Sscanf(mmdd, "%d/%d", &m, &d)
	y := end.Year()
	if m > int(end.Month()) {
		y--
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}

// between returns the lines from the first start to the next stop after it (or the end).
func between(text, start, stop string) []string {
	_, rest, ok := strings.Cut(text, start)
	if !ok {
		return nil
	}
	rest, _, _ = strings.Cut(rest, stop)
	return strings.Split(rest, "\n")
}

func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

// balances reads the opening and closing balance patterns, negated for a card (owed).
func balances(a *stmtAccount, text, opening, closing string, card bool) error {
	o, err1 := money(find(opening, text))
	c, err2 := money(find(closing, text))
	if card {
		o, c = -o, -c
	}
	a.Opening, a.Closing = o, c
	return errors.Join(err1, err2)
}

var (
	cardLine   = regexp.MustCompile(`^\s*(\d\d/\d\d)\s+(?:\d\d/\d\d\s+)?(.*?\S)\s+(-?\$?[\d,]*\.\d\d-?)\s*$`)
	moneyToken = regexp.MustCompile(`^-?\$?\(?[\d,]*\.\d\d\)?-?$`)
	refToken   = regexp.MustCompile(`^[A-Z0-9]{10,}$`)
)

// cardTxs reads "MM/DD [MM/DD] description amount" lines, where a charge is positive.
func cardTxs(a *stmtAccount, lines []string, end time.Time) error {
	for _, l := range lines {
		m := cardLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		v, err := money(m[3])
		if err != nil {
			return fmt.Errorf("%q: %w", l, err)
		}
		f := strings.Fields(m[2])
		if len(f) > 1 && refToken.MatchString(f[0]) && strings.ContainsAny(f[0], "0123456789") {
			f = f[1:] // USAA's reference number
		}
		a.Txs = append(a.Txs, stmtTx{inYear(m[1], end), -v, strings.Join(f, " ")})
	}
	return nil
}

func parseChaseCard(text string) (stmtAccount, error) {
	a := stmtAccount{Checked: true, Last4: find(`Account Number:\s+(?:X+\s+){3}(\d{4})`, text)}
	end, err := usDate(find(`Opening/Closing Date\s+\S+\s*-\s*(\S+)`, text))
	if err != nil {
		return a, fmt.Errorf("closing date: %w", err)
	}
	a.End = end.Format(time.DateOnly)
	if err := balances(&a, text, `Previous Balance\s+(\S+)`, `New Balance:?\s+(-?\$[\d,]+\.\d\d)`, true); err != nil {
		return a, err
	}
	return a, cardTxs(&a, between(text, "ACCOUNT ACTIVITY", "Totals Year-to-Date"), end)
}

func parseUSAACard(text string) (stmtAccount, error) {
	a := stmtAccount{Checked: true, Last4: find(`Account Number\s+\d{4} X+ X+ (\d{4})`, text)}
	end, err := usDate(find(`Statement Closing Date\s+(\S+)`, text))
	if err != nil {
		return a, fmt.Errorf("closing date: %w", err)
	}
	a.End = end.Format(time.DateOnly)
	if err := balances(&a, text, `Previous Balance\s+(\$\S+)`, `New Balance\s+(\$[\d,]+\.\d\d)`, true); err != nil {
		return a, err
	}
	return a, cardTxs(&a, between(text, "Payments and Credits", "Interest Charge Calculation"), end)
}

var runningLine = regexp.MustCompile(`^\s*(\d\d/\d\d)\s+(.*?\S)\s+(\$[\d,]*\d\.\d\d)\s*$`)

// parseUSAABank reads the layout from 2021 on: each line ends with the running balance, so a transaction's
// amount is its change in balance, and its second line (the payee) is added to the name.
func parseUSAABank(text string) (stmtAccount, error) {
	a := stmtAccount{Checked: true, Last4: find(`Account Number:\s+\d*(\d{4})`, text)}
	end, err := usDate(find(`Statement Period:\s+\S+ to (\S+)`, text))
	if err != nil {
		return a, fmt.Errorf("statement period: %w", err)
	}
	a.End = end.Format(time.DateOnly)
	if err := balances(&a, text, `Beginning Balance\s+(\$\S+)`, `Ending Balance\s+(\$\S+)`, false); err != nil {
		return a, err
	}
	prev, last := a.Opening, -1
	for _, l := range between(text, "\nTransactions", "\x00") {
		m := runningLine.FindStringSubmatch(l)
		if m == nil {
			if t := squeeze(l); last >= 0 && strings.HasPrefix(l, "  ") && strings.ContainsAny(t, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") &&
				!strings.HasPrefix(t, "*") && !strings.Contains(t, "continued") {
				a.Txs[last].Name += " " + t
			}
			last = -1
			continue
		}
		bal, err := money(m[3])
		if err != nil {
			return a, fmt.Errorf("%q: %w", l, err)
		}
		f := strings.Fields(m[2])
		for len(f) > 0 && moneyToken.MatchString(f[len(f)-1]) {
			f = f[:len(f)-1] // the debit or credit column
		}
		name := strings.Join(f, " ")
		if strings.Contains(name, "Beginning Balance") || strings.Contains(name, "Ending Balance") {
			prev, last = bal, -1
			continue
		}
		a.Txs = append(a.Txs, stmtTx{inYear(m[1], end), bal - prev, name})
		prev, last = bal, len(a.Txs)-1
	}
	return a, nil
}

var (
	oldLine    = regexp.MustCompile(`^\s*(\d\d/\d\d)\s+([\d,]*\.\d\d)\s+(.*\S)`)
	oldSection = regexp.MustCompile(`^\s{0,8}[A-Z][A-Z &]+$`)
	oldCheck   = regexp.MustCompile(`(\d\d/\d\d)\s+(\d+)\*?\s+([\d,]*\.\d\d)`) // up to two per line
)

// parseUSAAOld reads USAA's layout until early 2021: "MM/DD amount description" lines under section headings
// that give the sign, and a summary row with the opening and closing balances.
func parseUSAAOld(text string) (stmtAccount, error) {
	a := stmtAccount{Checked: true}
	if n := regexp.MustCompile(`\D`).ReplaceAllString(find(`(\d{5}-\d{4}-\d)`, text), ""); len(n) >= 4 {
		a.Last4 = n[len(n)-4:]
	}
	end, err := usDate(find(`(?m)-\s*(\d\d/\d\d/\d\d)\s*$`, text))
	if err != nil {
		return a, fmt.Errorf("statement period: %w", err)
	}
	a.End = end.Format(time.DateOnly)
	summary := strings.Fields(find(`LAST STATEMENT.*\n\s*([\d,. ]+?)\s*\n`, text))
	if len(summary) < 2 {
		return a, errors.New("no balance summary")
	}
	o, err1 := money(summary[0])
	c, err2 := money(summary[len(summary)-1])
	if err := errors.Join(err1, err2); err != nil {
		return a, err
	}
	a.Opening, a.Closing = o, c
	sign, last, checks := int64(0), -1, false
	for _, l := range strings.Split(text, "\n") {
		if oldSection.MatchString(l) {
			h := strings.TrimSpace(l)
			checks = h == "CHECKS"
			switch {
			case strings.Contains(h, "BALANCE SUMMARY") || strings.Contains(h, "OUTSTANDING"):
				sign = 0
			case strings.Contains(h, "DEBIT") || strings.Contains(h, "WITHDRAWAL") || strings.HasPrefix(h, "CHECKS"):
				sign = -1
			case strings.Contains(h, "DEPOSIT") || strings.Contains(h, "CREDIT"):
				sign = 1
			}
			last = -1
			continue
		}
		if checks {
			for _, c := range oldCheck.FindAllStringSubmatch(l, -1) {
				v, err := money(c[3])
				if err != nil {
					return a, fmt.Errorf("%q: %w", l, err)
				}
				a.Txs = append(a.Txs, stmtTx{inYear(c[1], end), -v, "CHECK " + c[2]})
			}
			continue
		}
		m := oldLine.FindStringSubmatch(l)
		if m == nil || sign == 0 {
			if last >= 0 && strings.HasPrefix(l, "          ") && strings.TrimSpace(l) != "" {
				a.Txs[last].Name += " " + squeeze(l)
			}
			last = -1
			continue
		}
		v, err := money(m[2])
		if err != nil {
			return a, fmt.Errorf("%q: %w", l, err)
		}
		a.Txs = append(a.Txs, stmtTx{inYear(m[1], end), sign * v, squeeze(m[3])})
		last = len(a.Txs) - 1
	}
	return a, nil
}

var (
	vgTx     = regexp.MustCompile(`^\s*(\d\d/\d\d)\s+\d\d/\d\d\s+(\S+)\s+(.*?)\s+(-?\$?[\d,]*\.\d\d)\s*$`)
	vgColumn = regexp.MustCompile(`^(-|Cash|Margin|\$?[\d,.]+)$`)
)

// parseVanguard reads a Vanguard statement (one per account): the closing value from its overview, and the
// settled activity. A buy or reinvestment is a negative row and its dividend a positive one, as Monarch has them.
func parseVanguard(text string) (stmtAccount, error) {
	a := stmtAccount{Last4: find(`account—\S*?(\d{4})\b`, text)}
	m := regexp.MustCompile(`Statement overview\s+(\$[\d,]+\.\d\d)\s*\n\s*Total value of all accounts as of (\w+ \d{1,2}, \d{4})`).FindStringSubmatch(text)
	if m == nil {
		return a, errors.New("no statement overview")
	}
	end, err := time.Parse("January 2, 2006", m[2])
	if err != nil {
		return a, fmt.Errorf("statement date: %w", err)
	}
	a.End = end.Format(time.DateOnly)
	if a.Closing, err = money(m[1]); err != nil {
		return a, err
	}
	for _, l := range strings.Split(text, "\n") {
		m := vgTx.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		v, err := money(m[4])
		if err != nil {
			return a, fmt.Errorf("%q: %w", l, err)
		}
		f := strings.Fields(m[3])
		for len(f) > 0 && vgColumn.MatchString(f[len(f)-1]) {
			f = f[:len(f)-1] // quantity, price, fees, account type
		}
		if m[2] != "-" {
			f = append([]string{m[2]}, f...)
		}
		a.Txs = append(a.Txs, stmtTx{inYear(m[1], end), v, strings.Join(f, " ")})
	}
	return a, nil
}

type stmtDoc struct{ Name, Text string }

func importStatementsCLI(ctx context.Context, db *sql.DB, args []string, out io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "import-statements" {
		return false, nil
	}
	fs := flag.NewFlagSet("import-statements", flag.ContinueOnError)
	account := fs.String("account", "", "account id or last digits for every file (for a card whose number changed)")
	dry := fs.Bool("dry-run", false, "report what would change and roll back")
	if err := fs.Parse(args[1:]); err != nil {
		return true, err
	}
	if fs.NArg() == 0 {
		return true, errors.New("give the statement PDFs to import")
	}
	bin := os.Getenv("PDFTOTEXT")
	if bin == "" {
		bin = "pdftotext"
	}
	var docs []stmtDoc
	for _, f := range fs.Args() {
		text, err := exec.CommandContext(ctx, bin, "-layout", f, "-").Output()
		if err != nil {
			return true, fmt.Errorf("%s: pdftotext: %w", f, err)
		}
		docs = append(docs, stmtDoc{filepath.Base(f), string(text)})
	}
	return true, importStatements(ctx, db, docs, *account, *dry, out)
}

// importStatements does the work in one database transaction (rolled back for a dry run), printing a line per file.
func importStatements(ctx context.Context, db *sql.DB, docs []stmtDoc, account string, dry bool, out io.Writer) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var accounts []ourAccount
	rows, err := tx.QueryContext(ctx, `SELECT id,name,mask FROM accounts`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a ourAccount
		if err := rows.Scan(&a.id, &a.name, &a.mask); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, a)
	}
	rows.Close()
	resolve := func(last4 string) string {
		if account != "" {
			for _, a := range accounts {
				if a.id == account {
					return a.id
				}
			}
			last4 = account
		}
		id, _ := matchMonarchAccount("(..."+last4+")", accounts)
		return id
	}

	// The account's other history: where it starts, and its rows to match against.
	type row struct {
		date   time.Time
		amount int64
		used   bool
	}
	have := map[string][]*row{}
	cutoff := map[string]string{}
	rows, err = tx.QueryContext(ctx, `SELECT account_id,date,amount FROM transactions WHERE id NOT LIKE 'stmt:%' ORDER BY date`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a, d string
		var amount int64
		if err := rows.Scan(&a, &d, &amount); err != nil {
			rows.Close()
			return err
		}
		t, _ := time.Parse(time.DateOnly, d)
		have[a] = append(have[a], &row{date: t, amount: amount})
		if cutoff[a] == "" {
			cutoff[a] = d
		}
	}
	rows.Close()
	covered := func(account string, t stmtTx) bool {
		d, _ := time.Parse(time.DateOnly, t.Date)
		for _, r := range have[account] {
			if !r.used && r.amount == t.Amount && r.date.Sub(d).Abs() <= 3*24*time.Hour {
				r.used = true
				return true
			}
		}
		return cutoff[account] != "" && t.Date >= cutoff[account]
	}

	var imported, skipped int
	for _, doc := range docs {
		parts, err := parseStatement(doc.Text)
		if err != nil {
			fmt.Fprintf(out, "  %s: SKIPPED, %v\n", doc.Name, err)
			skipped++
			continue
		}
		for _, p := range parts {
			id := resolve(p.Last4)
			if id == "" {
				fmt.Fprintf(out, "  %s: SKIPPED, no account ending %s (use --account)\n", doc.Name, p.Last4)
				skipped++
				continue
			}
			if p.Checked {
				sum := p.Opening
				for _, t := range p.Txs {
					sum += t.Amount
				}
				if sum != p.Closing {
					fmt.Fprintf(out, "  %s: SKIPPED, %d transactions come to %s, not the closing balance %s\n",
						doc.Name, len(p.Txs), centsText(sum), centsText(p.Closing))
					skipped++
					continue
				}
			}
			n, seen := 0, map[string]int{}
			for _, t := range p.Txs {
				if t.Amount == 0 || covered(id, t) { // $0 rows are interest summaries
					continue
				}
				k := fmt.Sprintf("%s|%s|%d|%s", id, t.Date, t.Amount, t.Name)
				seen[k]++
				sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d", k, seen[k])))
				if _, err := tx.ExecContext(ctx, `INSERT INTO transactions(id,account_id,date,amount,name) VALUES($1,$2,$3,$4,$5)
					ON CONFLICT(id) DO NOTHING`, "stmt:"+hex.EncodeToString(sum[:10]), id, t.Date, t.Amount, t.Name); err != nil {
					return err
				}
				n++
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO balances(account_id,date,current) VALUES($1,$2,$3) ON CONFLICT(account_id,date) DO NOTHING`,
				id, p.End, p.Closing); err != nil {
				return err
			}
			imported += n
			fmt.Fprintf(out, "  %s: %s to %s, %d of %d transactions new\n", doc.Name, id, p.End, n, len(p.Txs))
		}
	}
	if err := errors.Join(assignMerchantKeys(ctx, tx), matchTransfers(ctx, tx), applyRules(ctx, tx)); err != nil {
		return err
	}
	verb := "Imported"
	if dry {
		verb = "Would import (dry run, nothing saved)"
	}
	fmt.Fprintf(out, "%s %d transactions; %d statements skipped.\n", verb, imported, skipped)
	if dry {
		return nil
	}
	return tx.Commit()
}

func centsText(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s$%d.%02d", sign, c/100, c%100)
}
