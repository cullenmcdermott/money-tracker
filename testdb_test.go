package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
)

// openTestDB returns a migrated database in a fresh schema of the Postgres at TEST_DATABASE_URL, so tests stay
// isolated from each other. The schema is dropped when the test ends.
func openTestDB(t testing.TB) (*sql.DB, error) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("TEST_DATABASE_URL is not set: the tests need a Postgres. Run `just test`, which starts a throwaway one.")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		return nil, err
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	schema := "test_" + hex.EncodeToString(random[:])
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := openDB(u.String())
	t.Cleanup(func() {
		if db != nil {
			db.Close()
		}
		admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		admin.Close()
	})
	return db, err
}
