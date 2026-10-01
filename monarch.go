package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Monarch import: a one-time (re-runnable) load of Monarch Money's CSV exports into SimpleFIN's accounts.
//
//	money-tracker import-monarch --transactions Transactions.csv --balances Balances.csv \
//	    [--property "123 Example St=R1234567890"] [--dry-run]
//
// Accounts are matched by the last digits in Monarch's "(...1234)" suffix, else by name without that suffix.
// Transactions are imported only before each account's first SimpleFIN transaction, skipping any that SimpleFIN
// already has (same amount within 3 days, since Monarch dates by transaction and SimpleFIN by posting). In the
// overlap, Monarch's category is copied onto matching SimpleFIN rows that have none. Daily balances fill dates
// SimpleFIN has no balance for. Rows are keyed "monarch:<Id>", so a re-run updates instead of duplicating, and
// never overwrites a category chosen since.

// monarchCategories maps Monarch's categories onto this app's; names not listed are kept as they are. Transfer keeps
// unpaired transfers out of cash flow; the empty ones (buys and sells, Uncategorized) are left for Review.
var monarchCategories = map[string]string{
	"Restaurants & Bars": "Dining", "Coffee Shops": "Coffee Shops", "Groceries": "Groceries",
	"Shopping": "Shopping", "Clothing": "Shopping", "Electronics": "Shopping", "Furniture & Housewares": "Shopping",
	"Home Improvement": "Housing", "Mortgage": "Housing",
	"Gas": "Transportation", "Parking & Tolls": "Transportation", "Taxi & Ride Shares": "Transportation",
	"Public Transit": "Transportation", "Auto Maintenance": "Transportation", "Auto Payment": "Transportation",
	"Medical": "Health", "Dentist": "Health", "Fitness": "Health", "Pets": "Pets",
	"Entertainment & Recreation": "Entertainment", "Travel & Vacation": "Travel", "Hotels": "Travel",
	"Gas & Electric": "Utilities", "Water": "Utilities", "Internet & Cable": "Utilities", "Insurance": "Insurance",
	"Charity": "Donations", "Paychecks": "Income", "Other Income": "Income", "Interest": "Income",
	"Dividends & Capital Gains": "Income", "Miscellaneous": "Other", "Fun Money": "Other",
	"Financial Fees": "Fees", "Financial & Legal Services": "Fees", "Postage & Shipping": "Shopping",
	"Office Supplies & Expenses": "Work Expenses", "Business Utilities & Communication": "Work Expenses",
	"Business Insurance": "Work Expenses", "Business Auto Expenses": "Work Expenses", "Cash & ATM": "Cash & Checks", "Check": "Cash & Checks",
	"Transfer": "Transfer", "Credit Card Payment": "Transfer",
	"Uncategorized": "", "Loan Repayment": "", "Buy": "", "Sell": "",
}

func monarchCategory(c string) string {
	if m, ok := monarchCategories[c]; ok {
		return m
	}
	return c
}

var monarchSuffix = regexp.MustCompile(`\s*\(\.*([^()]*)\)\s*$`)

// monarchKey splits "ACME CORP 401(K) PLAN (...3333)" into the name without the suffix and the suffix's last 4.
func monarchKey(name string) (base, last4 string) {
	if m := monarchSuffix.FindStringSubmatch(name); m != nil {
		base, last4 = strings.TrimSpace(name[:len(name)-len(m[0])]), m[1]
	} else {
		base = strings.TrimSpace(name)
	}
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	return strings.ToLower(base), strings.ToLower(last4)
}

type ourAccount struct{ id, name, mask string }

// matchMonarchAccount finds the account a Monarch account name refers to: same last 4, else same name without suffix.
func matchMonarchAccount(name string, accounts []ourAccount) (string, bool) {
	base, last4 := monarchKey(name)
	for _, a := range accounts {
		_, l := monarchKey(a.name)
		if last4 != "" && (strings.EqualFold(a.mask, last4) || l == last4) {
			return a.id, true
		}
	}
	for _, a := range accounts {
		if b, _ := monarchKey(a.name); b == base {
			return a.id, true
		}
	}
	return "", false
}

type monarchReport struct {
	Accounts                          map[string]string // Monarch account -> our account id ("" = not matched)
	Imported, Covered, Unmatched, Bad int
	CategoriesCopied, Balances        int
	Property                          string
}

func importMonarchCLI(ctx context.Context, db *sql.DB, args []string, out io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "import-monarch" {
		return false, nil
	}
	fs := flag.NewFlagSet("import-monarch", flag.ContinueOnError)
	txFile := fs.String("transactions", "", "Monarch transactions CSV")
	balFile := fs.String("balances", "", "Monarch balances CSV (optional)")
	property := fs.String("property", "", `"<Monarch account name>=<county parcel number>" to import a home's value history`)
	dry := fs.Bool("dry-run", false, "report what would change and roll back")
	if err := fs.Parse(args[1:]); err != nil {
		return true, err
	}
	if *txFile == "" {
		return true, errors.New("--transactions is required")
	}
	read := func(path string) ([]map[string]string, error) {
		if path == "" {
			return nil, nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return readCSV(f)
	}
	txs, err := read(*txFile)
	if err != nil {
		return true, err
	}
	bals, err := read(*balFile)
	if err != nil {
		return true, err
	}
	r, err := importMonarch(ctx, db, txs, bals, *property, *dry)
	if err != nil {
		return true, err
	}
	names := make([]string, 0, len(r.Accounts))
	for n := range r.Accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		to := r.Accounts[n]
		if to == "" {
			to = "NOT MATCHED, skipped"
		}
		fmt.Fprintf(out, "  %s -> %s\n", n, to)
	}
	verb := "Imported"
	if *dry {
		verb = "Would import (dry run, nothing saved)"
	}
	fmt.Fprintf(out, "%s %d transactions; %d already in SimpleFIN; %d from unmatched accounts; %d unreadable rows.\n",
		verb, r.Imported, r.Covered, r.Unmatched, r.Bad)
	fmt.Fprintf(out, "Categories copied onto %d SimpleFIN transactions; %d daily balances added.\n", r.CategoriesCopied, r.Balances)
	if r.Property != "" {
		fmt.Fprintln(out, r.Property)
	}
	return true, nil
}

func readCSV(r io.Reader) ([]map[string]string, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]map[string]string, 0, len(rows)-1)
	for _, row := range rows[1:] {
		m := make(map[string]string, len(row))
		for i, h := range rows[0] {
			if i < len(row) {
				m[strings.TrimSpace(h)] = strings.TrimSpace(row[i])
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// importMonarch does the work in one database transaction (rolled back for a dry run).
func importMonarch(ctx context.Context, db *sql.DB, txs, bals []map[string]string, property string, dry bool) (monarchReport, error) {
	r := monarchReport{Accounts: map[string]string{}}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()

	var accounts []ourAccount
	rows, err := tx.QueryContext(ctx, `SELECT id,name,mask FROM accounts`)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var a ourAccount
		if err := rows.Scan(&a.id, &a.name, &a.mask); err != nil {
			rows.Close()
			return r, err
		}
		accounts = append(accounts, a)
	}
	rows.Close()

	propName, parcel, _ := strings.Cut(property, "=")
	propName, parcel = strings.TrimSpace(propName), strings.TrimSpace(parcel)
	propID := ""
	if propName != "" {
		propID = "property:" + parcel
		if _, err := tx.ExecContext(ctx, `INSERT INTO items(id) VALUES('manual') ON CONFLICT DO NOTHING`); err != nil {
			return r, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(id,item_id,institution,name,guessed_type,guess_confident,parcel) VALUES($1,'manual','County Assessor',$2,'property',true,$3)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name,parcel=excluded.parcel`, propID, propName, parcel); err != nil {
			return r, err
		}
	}
	accountFor := func(monarch string) string {
		if id, ok := r.Accounts[monarch]; ok {
			return id
		}
		id := ""
		if propName != "" && strings.EqualFold(monarch, propName) {
			id = propID
		} else {
			id, _ = matchMonarchAccount(monarch, accounts)
		}
		r.Accounts[monarch] = id
		return id
	}

	// SimpleFIN's own rows per account: the cutoff (first date) and what is left to match against.
	type sfRow struct {
		id, date, category string
		amount             int64
		used               bool
	}
	sf := map[string][]*sfRow{}
	cutoff := map[string]string{}
	rows, err = tx.QueryContext(ctx, `SELECT id,account_id,date,amount,user_category,pending FROM transactions WHERE id NOT LIKE 'monarch:%' ORDER BY date`)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var row sfRow
		var account string
		var pending bool
		if err := rows.Scan(&row.id, &account, &row.date, &row.amount, &row.category, &pending); err != nil {
			rows.Close()
			return r, err
		}
		sf[account] = append(sf[account], &row) // pending rows too: they settle into the same transaction
		if cutoff[account] == "" && !pending {
			cutoff[account] = row.date
		}
	}
	rows.Close()
	near := func(account, date string, amount int64) *sfRow {
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			return nil
		}
		var best *sfRow
		bestGap := 4 * 24 * time.Hour
		for _, s := range sf[account] {
			if s.used || s.amount != amount {
				continue
			}
			sd, _ := time.Parse("2006-01-02", s.date)
			gap := sd.Sub(d)
			if gap < 0 {
				gap = -gap
			}
			if gap <= 3*24*time.Hour && gap < bestGap {
				best, bestGap = s, gap
			}
		}
		return best
	}

	for _, m := range txs {
		account := accountFor(m["Account"])
		if account == "" {
			r.Unmatched++
			continue
		}
		amount, err := decimalCents(m["Amount"])
		date := m["Date"]
		if _, derr := time.Parse("2006-01-02", date); err != nil || derr != nil || m["Id"] == "" {
			r.Bad++
			continue
		}
		category := monarchCategory(m["Category"])
		if s := near(account, date, amount); s != nil {
			s.used = true
			r.Covered++
			// An earlier run may have imported it before SimpleFIN had it.
			if _, err := tx.ExecContext(ctx, `DELETE FROM transactions WHERE id=$1`, "monarch:"+m["Id"]); err != nil {
				return r, err
			}
			if category != "" && s.category == "" {
				if _, err := tx.ExecContext(ctx, `UPDATE transactions SET user_category=$1 WHERE id=$2 AND user_category=''`, category, s.id); err != nil {
					return r, err
				}
				r.CategoriesCopied++
			}
			continue
		}
		if c := cutoff[account]; c != "" && date >= c {
			r.Covered++ // inside SimpleFIN's history: SimpleFIN's copy wins even when the dates drift further
			continue
		}
		name := m["Original Statement"]
		if name == "" {
			name = m["Merchant"]
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO transactions(id,account_id,date,amount,name,merchant,user_category) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(id) DO UPDATE SET account_id=excluded.account_id,date=excluded.date,amount=excluded.amount,name=excluded.name,merchant=excluded.merchant,
				user_category=CASE WHEN transactions.user_category='' THEN excluded.user_category ELSE transactions.user_category END`,
			"monarch:"+m["Id"], account, date, amount, name, m["Merchant"], category); err != nil {
			return r, err
		}
		r.Imported++
	}

	// Balances: Monarch's daily values on dates SimpleFIN has none for. Monarch's first days are partial (accounts
	// were added over a few days), so its history starts at the latest first date of any account it has.
	start := ""
	firsts := map[string]string{}
	for _, b := range bals {
		if a := b["Account"]; firsts[a] == "" || b["Date"] < firsts[a] {
			firsts[a] = b["Date"]
		}
	}
	for _, d := range firsts {
		if d > start {
			start = d
		}
	}
	for _, b := range bals {
		account := accountFor(b["Account"])
		if account == "" || b["Date"] < start {
			continue
		}
		cents, err := decimalCents(b["Balance"])
		if err != nil {
			r.Bad++
			continue
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO balances(account_id,date,current) VALUES($1,$2,$3) ON CONFLICT(account_id,date) DO NOTHING`, account, b["Date"], cents)
		if err != nil {
			return r, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			r.Balances++
		}
	}
	// Accounts that SimpleFIN never gave a balance (the property) take the latest imported one.
	if _, err := tx.ExecContext(ctx, `UPDATE accounts a SET current=(SELECT current FROM balances b WHERE b.account_id=a.id ORDER BY date DESC LIMIT 1)
		WHERE a.current IS NULL`); err != nil {
		return r, err
	}

	if err := errors.Join(assignMerchantKeys(ctx, tx), matchTransfers(ctx, tx), applyRules(ctx, tx)); err != nil {
		return r, err
	}
	if propID != "" && parcel != "" {
		r.Property = refreshProperty(ctx, tx, propID, parcel)
	}
	if dry {
		return r, nil
	}
	return r, tx.Commit()
}
