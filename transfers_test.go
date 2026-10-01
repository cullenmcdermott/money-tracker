package main

import (
	"context"
	"database/sql"
	"testing"
)

func transferDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO items(id) VALUES('item');
		INSERT INTO accounts(id,item_id,name,guessed_type) VALUES('checking','item','Checking','depository'),('savings','item','Savings','depository')`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func transferRow(t *testing.T, db *sql.DB, id, account, date string, amount int64, pending int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO transactions(id,account_id,date,amount,name,pending) VALUES($1,$2,$3,$4,$5,$6)`, id, account, date, amount, id, pending != 0); err != nil {
		t.Fatal(err)
	}
}

func runTransferMatch(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := matchTransfers(context.Background(), tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func transferPeer(t *testing.T, db *sql.DB, id string) sql.NullString {
	t.Helper()
	var peer sql.NullString
	if err := db.QueryRow(`SELECT transfer_id FROM transactions WHERE id=$1`, id).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	return peer
}

func TestTransferPairAndCashflow(t *testing.T) {
	db := transferDB(t)
	transferRow(t, db, "out", "checking", "2026-09-01", -10000, 0)
	transferRow(t, db, "in", "savings", "2026-09-03", 10000, 0)
	transferRow(t, db, "other", "checking", "2026-09-03", -700, 0)
	runTransferMatch(t, db)
	if peer := transferPeer(t, db, "out"); !peer.Valid || peer.String != "in" {
		t.Fatalf("out transfer_id = %v", peer)
	}
	if peer := transferPeer(t, db, "in"); !peer.Valid || peer.String != "out" {
		t.Fatalf("in transfer_id = %v", peer)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cashflow`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cashflow has %d rows, want unrelated transaction only", count)
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM cashflow`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != "other" {
		t.Fatalf("cashflow row = %q, want other", id)
	}
}

func TestTransferRejectsIneligiblePairs(t *testing.T) {
	for _, tc := range []struct {
		name, inAccount, inDate string
		inAmount                int64
		outPending, inPending   int
	}{
		{"same account", "checking", "2026-09-02", 10000, 0, 0},
		{"six days", "savings", "2026-09-07", 10000, 0, 0},
		{"pending inflow", "savings", "2026-09-02", 10000, 0, 1},
		{"pending outflow", "savings", "2026-09-02", 10000, 1, 0},
		{"different amounts", "savings", "2026-09-02", 10001, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := transferDB(t)
			transferRow(t, db, "out", "checking", "2026-09-01", -10000, tc.outPending)
			transferRow(t, db, "in", tc.inAccount, tc.inDate, tc.inAmount, tc.inPending)
			runTransferMatch(t, db)
			for _, id := range []string{"out", "in"} {
				if peer := transferPeer(t, db, id); peer.Valid {
					t.Fatalf("%s transfer_id = %v", id, peer)
				}
			}
		})
	}
}

func TestTransferInflowTieOrder(t *testing.T) {
	db := transferDB(t)
	transferRow(t, db, "out", "checking", "2026-09-04", -10000, 0)
	transferRow(t, db, "later", "savings", "2026-09-05", 10000, 0)
	transferRow(t, db, "b", "savings", "2026-09-03", 10000, 0)
	transferRow(t, db, "a", "savings", "2026-09-03", 10000, 0)
	runTransferMatch(t, db)
	if peer := transferPeer(t, db, "out"); !peer.Valid || peer.String != "a" {
		t.Fatalf("out transfer_id = %v", peer)
	}
	for _, id := range []string{"b", "later"} {
		if peer := transferPeer(t, db, id); peer.Valid {
			t.Fatalf("%s transfer_id = %v", id, peer)
		}
	}
}

func TestTransferGreedyByOutflowDate(t *testing.T) {
	db := transferDB(t)
	transferRow(t, db, "early", "checking", "2026-09-01", -10000, 0)
	transferRow(t, db, "late", "checking", "2026-09-03", -10000, 0)
	transferRow(t, db, "in", "savings", "2026-09-03", 10000, 0)
	runTransferMatch(t, db)
	if peer := transferPeer(t, db, "early"); !peer.Valid || peer.String != "in" {
		t.Fatalf("early transfer_id = %v", peer)
	}
	if peer := transferPeer(t, db, "late"); peer.Valid {
		t.Fatalf("late transfer_id = %v", peer)
	}
	if peer := transferPeer(t, db, "in"); !peer.Valid || peer.String != "early" {
		t.Fatalf("in transfer_id = %v", peer)
	}
}

func TestTransferThreeDayBoundary(t *testing.T) {
	for _, tc := range []struct{ name, outDate, inDate string }{
		{"same month", "2026-09-01", "2026-09-04"},
		{"month boundary", "2026-02-27", "2026-03-02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := transferDB(t)
			transferRow(t, db, "out", "checking", tc.outDate, -10000, 0)
			transferRow(t, db, "in", "savings", tc.inDate, 10000, 0)
			runTransferMatch(t, db)
			if peer := transferPeer(t, db, "out"); !peer.Valid || peer.String != "in" {
				t.Fatalf("out transfer_id = %v", peer)
			}
		})
	}
}

func TestTransferSkipsSameAccountAlternative(t *testing.T) {
	db := transferDB(t)
	transferRow(t, db, "out", "checking", "2026-09-01", -10000, 0)
	transferRow(t, db, "same", "checking", "2026-09-01", 10000, 0)
	transferRow(t, db, "other", "savings", "2026-09-03", 10000, 0)
	runTransferMatch(t, db)
	if peer := transferPeer(t, db, "out"); !peer.Valid || peer.String != "other" {
		t.Fatalf("out transfer_id = %v", peer)
	}
	if peer := transferPeer(t, db, "same"); peer.Valid {
		t.Fatalf("same transfer_id = %v", peer)
	}
}

func TestTransferSameSignUnmatched(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount int64
	}{
		{"zero", 0},
		{"outflows", -10000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := transferDB(t)
			transferRow(t, db, "a", "checking", "2026-09-01", tc.amount, 0)
			transferRow(t, db, "b", "savings", "2026-09-01", tc.amount, 0)
			runTransferMatch(t, db)
			for _, id := range []string{"a", "b"} {
				if peer := transferPeer(t, db, id); peer.Valid {
					t.Fatalf("%s transfer_id = %v", id, peer)
				}
			}
		})
	}
}

func TestTransferKeepsExistingMatch(t *testing.T) {
	db := transferDB(t)
	transferRow(t, db, "out", "checking", "2026-09-01", -10000, 0)
	transferRow(t, db, "in", "savings", "2026-09-03", 10000, 0)
	runTransferMatch(t, db)
	runTransferMatch(t, db)
	for id, want := range map[string]string{"out": "in", "in": "out"} {
		if peer := transferPeer(t, db, id); !peer.Valid || peer.String != want {
			t.Fatalf("unchanged rerun: %s transfer_id = %v", id, peer)
		}
	}
	if _, err := db.Exec(`INSERT INTO accounts(id,item_id,name) VALUES('third','item','Third')`); err != nil {
		t.Fatal(err)
	}
	transferRow(t, db, "closer", "third", "2026-09-03", -10000, 0)
	runTransferMatch(t, db)
	if peer := transferPeer(t, db, "out"); !peer.Valid || peer.String != "in" {
		t.Fatalf("out transfer_id = %v", peer)
	}
	if peer := transferPeer(t, db, "in"); !peer.Valid || peer.String != "out" {
		t.Fatalf("in transfer_id = %v", peer)
	}
	if peer := transferPeer(t, db, "closer"); peer.Valid {
		t.Fatalf("closer transfer_id = %v", peer)
	}
}

func TestTransferClearsStalePair(t *testing.T) {
	db := transferDB(t)
	transferRow(t, db, "out", "checking", "2026-09-01", -10000, 0)
	transferRow(t, db, "in", "savings", "2026-09-03", 10000, 0)
	runTransferMatch(t, db)
	if _, err := db.Exec(`DELETE FROM transactions WHERE id='in'`); err != nil {
		t.Fatal(err)
	}
	runTransferMatch(t, db)
	if peer := transferPeer(t, db, "out"); peer.Valid {
		t.Fatalf("out transfer_id = %v", peer)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cashflow WHERE id='out'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("survivor cashflow count = %d", count)
	}
}

func TestTransferRejectsPartialPair(t *testing.T) {
	for _, blocked := range []string{"out", "in"} {
		t.Run(blocked, func(t *testing.T) {
			db := transferDB(t)
			transferRow(t, db, "out", "checking", "2026-09-01", -10000, 0)
			transferRow(t, db, "in", "savings", "2026-09-01", 10000, 0)
			// A trigger returning NULL silently skips that row's update, so matchTransfers sees zero rows affected.
			if _, err := db.Exec(`CREATE FUNCTION skip_transfer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
				CREATE TRIGGER skip_transfer BEFORE UPDATE OF transfer_id ON transactions
				FOR EACH ROW WHEN (OLD.id='` + blocked + `') EXECUTE FUNCTION skip_transfer()`); err != nil {
				t.Fatal(err)
			}
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := matchTransfers(context.Background(), tx); err == nil {
				tx.Rollback()
				t.Fatal("expected zero-row update error")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"out", "in"} {
				if peer := transferPeer(t, db, id); peer.Valid {
					t.Fatalf("%s transfer_id = %v after rollback", id, peer)
				}
			}
		})
	}
}
