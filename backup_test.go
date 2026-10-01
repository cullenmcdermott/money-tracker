package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func backupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("BACKUP_DIR", filepath.Join(t.TempDir(), "backups"))
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func seedBackup(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO items(id) VALUES('i')`,
		`INSERT INTO accounts(id,item_id,name,guessed_type,current) VALUES('a','i','Checking','depository',123456)`,
		`INSERT INTO balances(account_id,date,current) VALUES('a','2026-09-01',123456)`,
		`INSERT INTO transactions(id,account_id,date,amount,name,user_category,pending) VALUES
			('t1','a','2026-09-01',-1999,'Coffee, "large"','Dining',false),
			('t2','a','2026-09-02',500000,'Pay','',true)`,
		`INSERT INTO rules(pattern,category) VALUES('coffee','Dining'),('pay','Income')`,
		`INSERT INTO merchants(key,display_name,merged_into,category) VALUES('coffee','Coffee','','Dining')`,
		`INSERT INTO alerts(kind,key,transaction_id,reasons,dismissed_at) VALUES('new_merchant','t1','t1','["First charge"]',now()),('pace','2026-09:Dining',NULL,'["over 50% above usual"]',NULL)`,
		`INSERT INTO merchant_locations(merchant_key,base,source) VALUES('coffee',true,'owner')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// dumpRows returns every table as sorted JSON rows (generated columns included), for row-for-row comparison.
func dumpRows(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, tb := range backupTables {
		rows, err := db.Query(`SELECT row_to_json(t)::text FROM ` + tb + ` t ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out[tb] = append(out[tb], s)
		}
		rows.Close()
	}
	return out
}

func makeBackup(t *testing.T, db *sql.DB) string {
	t.Helper()
	name, err := runBackup(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(backupDir(), name)
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	src := backupTestDB(t)
	seedBackup(t, src)
	path := makeBackup(t, src)
	dst, err := openTestDB(t) // a second, fresh schema
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO items(id) VALUES('old')`); err != nil {
		t.Fatal(err)
	}
	safety, err := restoreLocked(context.Background(), dst, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(backupDir(), safety)); err != nil || !strings.HasSuffix(safety, "-pre-restore.tar.gz") {
		t.Fatalf("pre-restore backup missing: %q %v", safety, err)
	}
	if want, got := dumpRows(t, src), dumpRows(t, dst); !reflect.DeepEqual(want, got) {
		t.Fatalf("restored rows differ:\nwant %v\ngot  %v", want, got)
	}
	// The replaced row is gone, the generated column is recomputed, and the identity sequence moved past restored ids.
	var eff string
	if err := dst.QueryRow(`SELECT effective_category FROM transactions WHERE id='t1'`).Scan(&eff); err != nil || eff != "Dining" {
		t.Fatalf("effective_category %q %v", eff, err)
	}
	if _, err := dst.Exec(`INSERT INTO rules(pattern,category) VALUES('new','X')`); err != nil {
		t.Fatalf("identity sequence not advanced: %v", err)
	}
}

func TestBackupCoversAllTables(t *testing.T) {
	db := backupTestDB(t)
	rows, err := db.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name != 'schema_migrations'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		rows.Scan(&n)
		if !contains(backupTables, n) {
			t.Errorf("table %s is not in backupTables", n)
		}
	}
}

func TestBackupRetention(t *testing.T) {
	db := backupTestDB(t)
	t.Setenv("BACKUP_KEEP", "3")
	dir := backupDir()
	os.MkdirAll(dir, 0700)
	for _, n := range []string{"20260101T000001Z", "20260101T000002Z", "20260101T000003Z", "20260101T000004Z"} {
		os.WriteFile(filepath.Join(dir, "money-"+n+".tar.gz"), nil, 0600)
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0600) // unrelated files are never touched
	name, err := runBackup(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	got := backupFiles(dir)
	if len(got) != 3 || got[0] != name || got[2] != "money-20260101T000003Z.tar.gz" {
		t.Fatalf("kept %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("unrelated file removed")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestRestoreRefusesBadArchives(t *testing.T) {
	db := backupTestDB(t)
	seedBackup(t, db)
	before := dumpRows(t, db)
	good := makeBackup(t, db)
	dir := t.TempDir()

	// Manifest from a different schema version.
	m, data, err := snapshot(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion++
	var buf bytes.Buffer
	if err := writeArchive(&buf, m, data); err != nil {
		t.Fatal(err)
	}
	mismatch := filepath.Join(dir, "mismatch.tar.gz")
	os.WriteFile(mismatch, buf.Bytes(), 0600)

	goodBytes, _ := os.ReadFile(good)
	corrupt := filepath.Join(dir, "corrupt.tar.gz")
	os.WriteFile(corrupt, goodBytes[:len(goodBytes)/2], 0600) // truncated
	garbage := filepath.Join(dir, "garbage.tar.gz")
	os.WriteFile(garbage, []byte("not an archive"), 0600)
	for name, p := range map[string]string{"mismatch": mismatch, "corrupt": corrupt, "garbage": garbage} {
		files := len(backupFiles(backupDir()))
		if _, err := restoreLocked(context.Background(), db, p); !errors.Is(err, errInvalid) {
			t.Errorf("%s: want errInvalid, got %v", name, err)
		}
		if got := dumpRows(t, db); !reflect.DeepEqual(before, got) {
			t.Errorf("%s: data changed", name)
		}
		if len(backupFiles(backupDir())) != files {
			t.Errorf("%s: a safety backup was taken before validation passed", name)
		}
	}
	if _, err := restoreLocked(context.Background(), db, mismatch); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Errorf("mismatch message: %v", err)
	}
}

func TestNextBackupTime(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.Local)
	if got := nextBackupTime(now, "03:00"); !got.Equal(time.Date(2026, 9, 30, 3, 0, 0, 0, time.Local)) {
		t.Fatal(got)
	}
	if got := nextBackupTime(now, "23:30"); !got.Equal(time.Date(2026, 9, 29, 23, 30, 0, 0, time.Local)) {
		t.Fatal(got)
	}
}

func TestBackupAPI(t *testing.T) {
	db := backupTestDB(t)
	seedBackup(t, db)
	h := (&app{db: db}).routes()
	do := func(method, target string, body []byte) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, target, bytes.NewReader(body)))
		return w
	}
	type listing struct {
		Backups []struct {
			Name, CreatedAt string
			Size            int64
		}
		LastBackup string `json:"last_backup"`
		LastError  string `json:"last_error"`
		Writable   bool
		Stale      bool
	}
	list := func() (l listing) {
		w := do("GET", "/api/backups", nil)
		if w.Code != 200 {
			t.Fatalf("list: %d %s", w.Code, w.Body)
		}
		json.NewDecoder(w.Body).Decode(&l)
		return
	}
	// Decoding is case-insensitive, so CreatedAt <- created_at needs the underscore-free field; check raw JSON too.
	if l := list(); len(l.Backups) != 0 || !l.Writable || l.Stale {
		t.Fatalf("empty list: %+v", l)
	}
	if w := do("POST", "/api/backups", nil); w.Code != 200 {
		t.Fatalf("post: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/api/backups", nil); !strings.Contains(w.Body.String(), `"created_at"`) || !strings.Contains(w.Body.String(), `"size"`) {
		t.Fatalf("list json: %s", w.Body)
	}
	l := list()
	if len(l.Backups) != 1 || l.Backups[0].Size == 0 || l.LastBackup == "" {
		t.Fatalf("list: %+v", l)
	}
	name := l.Backups[0].Name
	os.WriteFile(filepath.Join(filepath.Dir(backupDir()), "secret.tar.gz"), []byte("x"), 0600)

	// Restore by name: wipe the data first, then the restore brings it back.
	want := dumpRows(t, db)
	db.Exec(`DELETE FROM items`)
	if w := do("POST", "/api/backups/"+name+"/restore", nil); w.Code != 200 {
		t.Fatalf("restore by name: %d %s", w.Code, w.Body)
	}
	if got := dumpRows(t, db); !reflect.DeepEqual(want, got) {
		t.Fatal("restore by name did not bring the rows back")
	}
	if n := len(backupFiles(backupDir())); n != 2 { // original + pre-restore
		t.Fatalf("want a pre-restore backup, files: %v", backupFiles(backupDir()))
	}

	// Upload restore through the handler.
	raw, _ := os.ReadFile(filepath.Join(backupDir(), name))
	db.Exec(`DELETE FROM items`)
	if w := do("POST", "/api/backups/restore", raw); w.Code != 200 {
		t.Fatalf("upload restore: %d %s", w.Code, w.Body)
	}
	if got := dumpRows(t, db); !reflect.DeepEqual(want, got) {
		t.Fatal("upload restore did not bring the rows back")
	}
	if entries, _ := os.ReadDir(backupDir()); func() bool {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".upload-") {
				return true
			}
		}
		return false
	}() {
		t.Fatal("upload temp file not removed")
	}
	// A corrupt upload is a 400 and leaves data alone.
	if w := do("POST", "/api/backups/restore", raw[:len(raw)/2]); w.Code != http.StatusBadRequest {
		t.Fatalf("corrupt upload: %d %s", w.Code, w.Body)
	}
	if got := dumpRows(t, db); !reflect.DeepEqual(want, got) {
		t.Fatal("corrupt upload changed data")
	}
	// An oversized upload is refused.
	t.Setenv("BACKUP_MAX_UPLOAD", "10")
	if w := do("POST", "/api/backups/restore", raw); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d %s", w.Code, w.Body)
	}
	t.Setenv("BACKUP_MAX_UPLOAD", "")

	// Bad names never reach the filesystem.
	for _, bad := range []string{"..", "%2e%2e", "..%2Fsecret.tar.gz", "%2e%2e%2fsecret.tar.gz", "../secret.tar.gz", name + "%2F..%2F..%2Fsecret.tar.gz",
		"money-2026.tar.gz", name + ".bak", "secret.tar.gz", "x/" + name} {
		if w := do("POST", "/api/backups/"+bad+"/restore", nil); w.Code == http.StatusOK {
			t.Errorf("%q restored with 200", bad)
		}
	}
	for _, bad := range []string{"../x", "a/b", `..\x`, name + "/x", "money-2026010aT000001Z.tar.gz", "money-20260101T000001Z.tar.gz.bak", ""} {
		if backupName.MatchString(bad) {
			t.Errorf("%q passed validation", bad)
		}
	}
	if w := do("POST", "/api/backups/money-20260101T000001Z.tar.gz/restore", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing file: %d", w.Code)
	}

	// Only one backup or restore at a time.
	bk.run.Lock()
	for _, c := range []struct{ m, p string }{{"POST", "/api/backups"}, {"POST", "/api/backups/" + name + "/restore"}, {"POST", "/api/backups/restore"}} {
		if w := do(c.m, c.p, raw); w.Code != http.StatusConflict {
			t.Errorf("%s %s while busy: %d", c.m, c.p, w.Code)
		}
	}
	bk.run.Unlock()
}

func TestBackupListWarnings(t *testing.T) {
	db := backupTestDB(t)
	dir := backupDir()
	os.MkdirAll(dir, 0700)
	old := time.Now().Add(-40 * time.Hour).UTC().Format("20060102T150405Z")
	os.WriteFile(filepath.Join(dir, "money-"+old+".tar.gz"), []byte("x"), 0600)
	h := (&app{db: db}).routes()
	get := func() (out struct {
		Stale    bool
		Writable bool
	}) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/backups", nil))
		json.NewDecoder(w.Body).Decode(&out)
		return
	}
	if o := get(); !o.Stale || !o.Writable {
		t.Fatalf("old backup should be stale: %+v", o)
	}
	// BACKUP_DIR under a regular file cannot be created or written.
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0600)
	t.Setenv("BACKUP_DIR", filepath.Join(file, "sub"))
	if o := get(); o.Writable {
		t.Fatal("unwritable dir reported writable")
	}
}
