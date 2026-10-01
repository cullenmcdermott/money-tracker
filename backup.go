package main

// Backups: an in-process logical dump (the image is distroless, so there is no pg_dump). Every app table is copied
// with COPY ... TO STDOUT (CSV) inside one REPEATABLE READ read-only transaction, so the snapshot is consistent, and
// written into a gzipped tar with a manifest. Restore truncates the app tables and COPYs the CSVs back in one transaction.
// The dump holds all financial data, so treat BACKUP_DIR as sensitive (credentials are not in the database).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// backupTables is in foreign-key order (parents first). TestBackupCoversAllTables fails if a migration adds a table missing here.
var backupTables = []string{"items", "accounts", "balances", "transactions", "rules", "merchants", "simplefin_requests", "jev_suggestions", "jev_usage", "holdings"}

// backupName matches every file we write (nightly, on demand, and the safety copy taken before a restore).
var backupName = regexp.MustCompile(`^money-\d{8}T\d{6}Z(-pre-restore)?\.tar\.gz$`)

const staleAfter = 36 * time.Hour

// errInvalid marks a backup file that failed validation; the API reports it as a 400 and nothing has been touched.
var errInvalid = errors.New("invalid backup")

type backupManifest struct {
	Format        int                  `json:"format"`
	SchemaVersion int                  `json:"schema_version"`
	CreatedAt     string               `json:"created_at"`
	Tables        map[string]tableInfo `json:"tables"`
}

type tableInfo struct {
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
}

var bk struct {
	run     sync.Mutex // one backup or restore at a time
	mu      sync.Mutex // guards the fields below
	lastErr string
	next    time.Time
	started time.Time
}

func init() { bk.started = time.Now() }

func backupDir() string { return env("BACKUP_DIR", "data/backups") }

func backupKeep() int {
	if n, err := strconv.Atoi(env("BACKUP_KEEP", "14")); err == nil && n > 0 {
		return n
	}
	return 14
}

func maxUpload() int64 {
	if n, err := strconv.ParseInt(env("BACKUP_MAX_UPLOAD", ""), 10, 64); err == nil && n > 0 {
		return n
	}
	return 1 << 30
}

func schemaVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (v int, err error) {
	err = q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&v)
	return
}

// tableColumns lists a table's writable columns: generated ones (effective_category) are recomputed, so never dumped.
func tableColumns(ctx context.Context, conn *sql.Conn, table string) (cols []string, identity string, err error) {
	rows, err := conn.QueryContext(ctx, `SELECT column_name, is_identity = 'YES' FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND is_generated = 'NEVER' ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		var ident bool
		if err := rows.Scan(&c, &ident); err != nil {
			return nil, "", err
		}
		cols = append(cols, c)
		if ident {
			identity = c
		}
	}
	if len(cols) == 0 {
		return nil, "", fmt.Errorf("table %s not found", table)
	}
	return cols, identity, rows.Err()
}

func rawConn(ctx context.Context, conn *sql.Conn, fn func(*pgconn.PgConn) error) error {
	return conn.Raw(func(dc any) error { return fn(dc.(*stdlib.Conn).Conn().PgConn()) })
}

// snapshot dumps every table into memory inside one consistent transaction.
// ponytail: whole dump is held in memory (fine for a personal database); spool to temp files if it ever gets big.
func snapshot(ctx context.Context, db *sql.DB) (*backupManifest, map[string][]byte, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return nil, nil, err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
	version, err := schemaVersion(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	m := &backupManifest{Format: 1, SchemaVersion: version, CreatedAt: time.Now().UTC().Format(time.RFC3339), Tables: map[string]tableInfo{}}
	data := map[string][]byte{}
	for _, t := range backupTables {
		cols, _, err := tableColumns(ctx, conn, t)
		if err != nil {
			return nil, nil, err
		}
		var buf bytes.Buffer
		err = rawConn(ctx, conn, func(pc *pgconn.PgConn) error {
			tag, err := pc.CopyTo(ctx, &buf, fmt.Sprintf(`COPY (SELECT %s FROM %s) TO STDOUT WITH (FORMAT csv, HEADER true)`, strings.Join(cols, ","), t))
			m.Tables[t] = tableInfo{Columns: cols, Rows: tag.RowsAffected()}
			return err
		})
		if err != nil {
			return nil, nil, fmt.Errorf("dump %s: %w", t, err)
		}
		data[t] = buf.Bytes()
	}
	return m, data, nil
}

func writeArchive(w io.Writer, m *backupManifest, data map[string][]byte) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	put := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := put("manifest.json", mb); err != nil {
		return err
	}
	for _, t := range backupTables {
		if err := put(t+".csv", data[t]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// backupLocked writes one backup atomically (temp file, then rename) and prunes old ones. Caller holds bk.run.
func backupLocked(ctx context.Context, db *sql.DB, suffix string) (name string, err error) {
	defer func() {
		bk.mu.Lock()
		bk.lastErr = ""
		if err != nil {
			bk.lastErr = err.Error()
			log.Printf("backup: %v", err)
		}
		bk.mu.Unlock()
	}()
	dir := backupDir()
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	m, data, err := snapshot(ctx, db)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".backup-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	err = writeArchive(tmp, m, data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	name = "money-" + time.Now().UTC().Format("20060102T150405Z") + suffix + ".tar.gz"
	if err = os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return "", err
	}
	prune(dir, backupKeep())
	return name, nil
}

// runBackup waits for any running backup or restore, then backs up (the scheduler's entry point).
func runBackup(ctx context.Context, db *sql.DB) (string, error) {
	bk.run.Lock()
	defer bk.run.Unlock()
	return backupLocked(ctx, db, "")
}

// backupFiles returns our backup names, newest first (the timestamp sorts lexically).
func backupFiles(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if backupName.MatchString(e.Name()) && !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

func prune(dir string, keep int) {
	names := backupFiles(dir)
	for i := keep; i < len(names); i++ {
		if err := os.Remove(filepath.Join(dir, names[i])); err != nil {
			log.Printf("backup prune: %v", err)
		}
	}
}

// backupTime parses the UTC timestamp out of a backup file name.
func backupTime(name string) time.Time {
	t, _ := time.Parse("20060102T150405Z", name[len("money-"):len("money-")+16])
	return t
}

// nextBackupTime returns the next occurrence of HH:MM local time after now.
func nextBackupTime(now time.Time, hhmm string) time.Time {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		log.Printf("BACKUP_TIME %q is not HH:MM; using 03:00", hhmm)
		t, _ = time.Parse("15:04", "03:00")
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// backupLoop runs runBackup daily at BACKUP_TIME (server local time) until ctx is done.
func (a *app) backupLoop(ctx context.Context) {
	for {
		next := nextBackupTime(time.Now(), env("BACKUP_TIME", "03:00"))
		bk.mu.Lock()
		bk.next = next
		bk.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		if _, err := runBackup(ctx, a.db); err != nil && errors.Is(err, context.Canceled) {
			return
		}
	}
}

// walkArchive streams the archive's entries in order. Reading a tar to EOF also verifies the gzip checksum.
func walkArchive(path string, fn func(name string, r io.Reader) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%w: not a gzip file", errInvalid)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errInvalid, err)
		}
		if err := fn(h.Name, tr); err != nil {
			return err
		}
	}
}

// validateArchive checks everything that can be checked without touching the database contents: the archive parses
// end to end, the manifest comes first, only known table files follow in FK order, and the schema matches.
func validateArchive(ctx context.Context, db *sql.DB, path string) error {
	var m backupManifest
	var order []string
	err := walkArchive(path, func(name string, r io.Reader) error {
		if len(order) == 0 && m.Format == 0 {
			if name != "manifest.json" {
				return fmt.Errorf("%w: manifest.json must be first", errInvalid)
			}
			if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&m); err != nil || m.Format != 1 {
				return fmt.Errorf("%w: unreadable manifest", errInvalid)
			}
			return nil
		}
		t := strings.TrimSuffix(name, ".csv")
		if t == name || !contains(backupTables, t) {
			return fmt.Errorf("%w: unexpected file %q", errInvalid, name)
		}
		order = append(order, t)
		_, err := io.Copy(io.Discard, r)
		if err != nil {
			return fmt.Errorf("%w: %v", errInvalid, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if m.Format == 0 {
		return fmt.Errorf("%w: manifest.json missing", errInvalid)
	}
	if !equal(order, backupTables) {
		return fmt.Errorf("%w: tables are %v, want %v", errInvalid, order, backupTables)
	}
	cur, err := schemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if m.SchemaVersion != cur {
		return fmt.Errorf("%w: backup is schema version %d but the database is at %d", errInvalid, m.SchemaVersion, cur)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, t := range backupTables {
		cols, _, err := tableColumns(ctx, conn, t)
		if err != nil {
			return err
		}
		if !equal(cols, m.Tables[t].Columns) {
			return fmt.Errorf("%w: columns of %s differ from the current schema", errInvalid, t)
		}
	}
	return nil
}

// restoreArchive replaces all app data with the archive's in one transaction: any failure rolls back completely.
func restoreArchive(ctx context.Context, db *sql.DB, path string) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN`); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()
	if _, err = conn.ExecContext(ctx, `TRUNCATE `+strings.Join(backupTables, ", ")); err != nil {
		return err
	}
	cols := map[string][]string{}
	identity := map[string]string{}
	for _, t := range backupTables {
		if cols[t], identity[t], err = tableColumns(ctx, conn, t); err != nil {
			return err
		}
	}
	err = walkArchive(path, func(name string, r io.Reader) error {
		t := strings.TrimSuffix(name, ".csv")
		if t == name { // manifest.json
			return nil
		}
		return rawConn(ctx, conn, func(pc *pgconn.PgConn) error {
			_, err := pc.CopyFrom(ctx, r, fmt.Sprintf(`COPY %s(%s) FROM STDIN WITH (FORMAT csv, HEADER true)`, t, strings.Join(cols[t], ",")))
			if err != nil {
				return fmt.Errorf("restore %s: %w", t, err)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	for _, t := range backupTables { // COPY wrote explicit ids: move identity sequences past them
		if c := identity[t]; c != "" {
			if _, err = conn.ExecContext(ctx, fmt.Sprintf(`SELECT setval(pg_get_serial_sequence('%s','%s'), COALESCE(MAX(%s),0)+1, false) FROM %s`, t, c, c, t)); err != nil {
				return err
			}
		}
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

// restoreLocked is the one restore path (API by name, API upload, CLI): validate, take a "-pre-restore" safety backup
// (abort if that fails), then replace the data. Caller holds bk.run. Returns the safety backup's name.
func restoreLocked(ctx context.Context, db *sql.DB, path string) (string, error) {
	if err := validateArchive(ctx, db, path); err != nil {
		return "", err
	}
	safety, err := backupLocked(ctx, db, "-pre-restore")
	if err != nil {
		return "", fmt.Errorf("safety backup failed, nothing was changed: %w", err)
	}
	if err := restoreArchive(ctx, db, path); err != nil {
		return safety, fmt.Errorf("restore failed, nothing was changed: %w", err)
	}
	return safety, nil
}

func writable(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".probe-*.tmp")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

func (a *app) backupRoutes(mux *http.ServeMux) {
	fail := func(w http.ResponseWriter, err error) {
		status := http.StatusInternalServerError
		var tooBig *http.MaxBytesError
		switch {
		case errors.Is(err, errInvalid):
			status = http.StatusBadRequest
		case errors.As(err, &tooBig):
			status = http.StatusRequestEntityTooLarge
		}
		jsonResponse(w, status, map[string]string{"error": err.Error()})
	}
	busy := func(w http.ResponseWriter) {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": "a backup or restore is already running"})
	}
	mux.HandleFunc("GET /api/backups", func(w http.ResponseWriter, r *http.Request) {
		dir := backupDir()
		list := []map[string]any{}
		var last time.Time
		for _, n := range backupFiles(dir) {
			info, err := os.Stat(filepath.Join(dir, n))
			if err != nil {
				continue
			}
			created := backupTime(n)
			if last.IsZero() {
				last = created
			}
			list = append(list, map[string]any{"name": n, "size": info.Size(), "created_at": created.Format(time.RFC3339)})
		}
		out := map[string]any{"backups": list, "last_backup": "", "writable": true, "write_error": ""}
		if !last.IsZero() {
			out["last_backup"] = last.Format(time.RFC3339)
		}
		if err := writable(dir); err != nil {
			out["writable"], out["write_error"] = false, err.Error()
		}
		// Stale: newest backup older than 36h, or none yet although the server has been up that long.
		ref := last
		if ref.IsZero() {
			ref = bk.started
		}
		out["stale"] = time.Since(ref) > staleAfter
		bk.mu.Lock()
		out["last_error"] = bk.lastErr
		out["next_run"] = ""
		if !bk.next.IsZero() {
			out["next_run"] = bk.next.Format(time.RFC3339)
		}
		bk.mu.Unlock()
		jsonResponse(w, 200, out)
	})
	mux.HandleFunc("POST /api/backups", func(w http.ResponseWriter, r *http.Request) {
		if !bk.run.TryLock() {
			busy(w)
			return
		}
		defer bk.run.Unlock()
		name, err := backupLocked(context.WithoutCancel(r.Context()), a.db, "")
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"name": name})
	})
	mux.HandleFunc("POST /api/backups/{name}/restore", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !backupName.MatchString(name) { // strict allowlist: no separators, nothing beyond the fixed pattern
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid backup name"})
			return
		}
		path := filepath.Join(backupDir(), name)
		if _, err := os.Stat(path); err != nil {
			jsonResponse(w, http.StatusNotFound, map[string]string{"error": "backup not found"})
			return
		}
		if !bk.run.TryLock() {
			busy(w)
			return
		}
		defer bk.run.Unlock()
		safety, err := restoreLocked(context.WithoutCancel(r.Context()), a.db, path)
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"restored": name, "safety_backup": safety})
	})
	mux.HandleFunc("POST /api/backups/restore", func(w http.ResponseWriter, r *http.Request) {
		if !bk.run.TryLock() {
			busy(w)
			return
		}
		defer bk.run.Unlock()
		dir := backupDir()
		if err := os.MkdirAll(dir, 0700); err != nil {
			fail(w, err)
			return
		}
		tmp, err := os.CreateTemp(dir, ".upload-*.tmp")
		if err != nil {
			fail(w, err)
			return
		}
		defer os.Remove(tmp.Name())
		// The server's 30s ReadTimeout is sized for JSON; give an upload as long as the write side allows.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Minute))
		_, err = io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxUpload()))
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			fail(w, err)
			return
		}
		safety, err := restoreLocked(context.WithoutCancel(r.Context()), a.db, tmp.Name())
		if err != nil {
			fail(w, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"restored": "upload", "safety_backup": safety})
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// restoreCLI handles `money-tracker restore <file> --yes`; it reports whether args were a restore command.
// The database comes from DATABASE_URL; --yes is required because the current data is replaced.
func restoreCLI(db *sql.DB, args []string) bool {
	if len(args) == 0 || args[0] != "restore" {
		return false
	}
	var file string
	yes := false
	for _, a := range args[1:] {
		if a == "--yes" {
			yes = true
		} else if file == "" {
			file = a
		}
	}
	if file == "" {
		log.Fatal("usage: money-tracker restore <backup.tar.gz> --yes")
	}
	if !yes {
		log.Fatalf("restore replaces ALL current data with %s; re-run with --yes to confirm", file)
	}
	bk.run.Lock()
	safety, err := restoreLocked(context.Background(), db, file)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("restored %s (safety backup: %s).", file, safety)
	return true
}
