package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// applyRules sets rule_category on every transaction without a manual category: the matching rule, else the
// "remembered" category of its merchant, else ”. Precedence: manual > rule > merchant category > uncategorized.
// When several rules match, the longest pattern wins (the most specific), then the oldest rule. Matching is
// case-insensitive (lower() folds per the database locale). Each name and pattern is lowercased once, and only rows
// whose category changes are written, so a sync doesn't rewrite every transaction.
func applyRules(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) error {
	_, err := db.ExecContext(ctx, `WITH r AS MATERIALIZED (SELECT lower(pattern) p,category,length(pattern) l,id FROM rules),
		t AS (SELECT id,lower(COALESCE(NULLIF(merchant,''),name)) n,merchant_key FROM transactions WHERE user_category=''),
		c AS (SELECT t.id,COALESCE(NULLIF((SELECT r.category FROM r WHERE strpos(t.n,r.p)>0 ORDER BY r.l DESC,r.id LIMIT 1),''),
			(SELECT category FROM merchant_roots WHERE key=t.merchant_key),'') cat FROM t)
		UPDATE transactions SET rule_category=c.cat FROM c WHERE transactions.id=c.id AND transactions.rule_category<>c.cat`)
	return err
}

// transferWindow is how far apart the two sides of a transfer may be dated. Five days, because banks date a card
// payment or a brokerage move by different conventions (Monarch's history showed 4-day gaps); the amounts must
// still match to the cent, in opposite directions, between two of your accounts.
const transferWindow = 5 * 24 * time.Hour

// matchTransfers pairs transactions that move money between own accounts and sets their transfer_id.
func matchTransfers(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE transactions SET transfer_id=NULL
		WHERE transfer_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM transactions o WHERE o.id=transactions.transfer_id)`); err != nil {
		return err
	}

	type candidate struct {
		id, account string
		date        time.Time
		amount      int64
	}

	// ponytail: scan all unmatched transactions; limit to recent dates if personal-scale history makes sync slow.
	rows, err := tx.QueryContext(ctx, `SELECT id,account_id,date,amount FROM transactions
		WHERE NOT pending AND transfer_id IS NULL ORDER BY date,id`)
	if err != nil {
		return err
	}
	var candidates []candidate
	inflows := make(map[int64][]int)
	var outflows []int
	for rows.Next() {
		var c candidate
		var date string
		if err := rows.Scan(&c.id, &c.account, &date, &c.amount); err != nil {
			rows.Close()
			return err
		}
		c.date, err = time.Parse("2006-01-02", date)
		if err != nil {
			rows.Close()
			return fmt.Errorf("transfer %s date: %w", c.id, err)
		}
		index := len(candidates)
		candidates = append(candidates, c)
		if c.amount > 0 {
			inflows[c.amount] = append(inflows[c.amount], index)
		} else if c.amount < 0 {
			outflows = append(outflows, index)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, `UPDATE transactions SET transfer_id=$1 WHERE id=$2 AND transfer_id IS NULL`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	matched := make([]bool, len(candidates))
	for _, outIndex := range outflows {
		out := candidates[outIndex]
		bestIndex := -1
		bestDiff := transferWindow + 1
		for _, inIndex := range inflows[-out.amount] {
			in := candidates[inIndex]
			if matched[inIndex] || in.account == out.account {
				continue
			}
			diff := out.date.Sub(in.date)
			if diff < 0 {
				diff = -diff
			}
			if diff <= transferWindow && diff < bestDiff {
				bestIndex, bestDiff = inIndex, diff
			}
		}
		if bestIndex < 0 {
			continue
		}
		matched[bestIndex] = true
		in := candidates[bestIndex]
		result, err := stmt.ExecContext(ctx, in.id, out.id)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return fmt.Errorf("update transfer %s: affected %d rows: %v", out.id, affected, err)
		}
		result, err = stmt.ExecContext(ctx, out.id, in.id)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return fmt.Errorf("update transfer %s: affected %d rows: %v", in.id, affected, err)
		}
	}
	return nil
}
