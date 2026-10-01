package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed all:web/dist
var site embed.FS

type app struct {
	db        *sql.DB
	simplefin simplefinClient
	jev       *jevClient // nil = Jev off
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func apiError(w http.ResponseWriter, err error) {
	log.Printf("api error: %v", err) // never echo internal errors to clients
	jsonResponse(w, http.StatusBadGateway, map[string]string{"error": "upstream request failed"})
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(value); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return false
	}
	return true
}

func queryRows(w http.ResponseWriter, rows *sql.Rows, err error, scan func(*sql.Rows) (any, error)) {
	if err != nil {
		apiError(w, err)
		return
	}
	defer rows.Close()
	result := make([]any, 0)
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			apiError(w, err)
			return
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}

// validPeriod reports whether s is YYYY-MM or YYYY.
func validPeriod(s string) bool {
	layout := map[int]string{4: "2006", 7: "2006-01"}[len(s)]
	_, err := time.Parse(layout, s)
	return layout != "" && err == nil
}

// investedByMonth is the net money moved from cash and credit accounts into investment (and other) accounts, so a
// withdrawal back to checking counts negative: matched transfer pairs, plus unpaired Transfer-category rows whose
// description names an institution you have an investment account at (history whose other side is missing).
func (a *app) investedByMonth(ctx context.Context) (map[string]int64, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT substr(t.date,1,7), -SUM(t.amount) FROM transactions t
		JOIN accounts a ON a.id=t.account_id LEFT JOIN transactions o ON o.id=t.transfer_id LEFT JOIN accounts oa ON oa.id=o.account_id
		WHERE NOT t.pending AND a.type IN ('depository','credit') AND (oa.type IN ('investment','other')
			OR (t.transfer_id IS NULL AND t.effective_category='Transfer' AND EXISTS (SELECT 1 FROM accounts ia
				WHERE ia.type IN ('investment','other') AND strpos(lower(t.name), lower(`+institutionWordSQL+`))>0)))
		GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var month string
		var amount int64
		if err := rows.Scan(&month, &amount); err != nil {
			return nil, err
		}
		out[month] = amount
	}
	return out, rows.Err()
}

// institutionWordSQL is the distinctive word of an institution name, as bank descriptions spell it: "Contoso Bank US"
// -> "contoso", "Fabrikam Investments" -> "fabrikam", "Northwind" -> "northwind".
const institutionWordSQL = `COALESCE((SELECT w FROM regexp_split_to_table(lower(ia.institution), '\s+') w
	WHERE w NOT IN ('charles','bank','us','the','investments','financial','group','of','and','na') AND length(w) > 2 LIMIT 1), '~')`

func (a *app) monthly(ctx context.Context) ([]map[string]any, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT substr(date,1,7),
		COALESCE(SUM(CASE WHEN amount>0 THEN amount END),0),
		COALESCE(-SUM(CASE WHEN amount<0 THEN amount END),0)
		FROM cashflow GROUP BY substr(date,1,7) ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	invested, err := a.investedByMonth(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0)
	for rows.Next() {
		var month string
		var income, expense int64
		if err := rows.Scan(&month, &income, &expense); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"month": month, "income": income, "expense": expense, "invested": invested[month]})
	}
	return result, rows.Err()
}

// syncNow syncs the SimpleFIN connection; spendSimplefinRequest keeps repeated clicks within Bridge's daily budget.
func (a *app) syncNow(ctx context.Context) error {
	if a.simplefin.accessURL == "" {
		return errors.New("SimpleFIN is not configured; set SIMPLEFIN_ACCESS_URL")
	}
	return a.syncSimplefin(ctx)
}

// syncDue: the last successful sync is over 23 hours old (SimpleFIN refreshes from banks about daily), and
// nothing was requested in the last 4 hours, so a failing sync retries a few times a day, not every hour.
func syncDue(ctx context.Context, db *sql.DB) (bool, error) {
	var due bool
	err := db.QueryRowContext(ctx, `SELECT COALESCE((SELECT last_synced_at::timestamptz < now() - interval '23 hours' FROM items WHERE id=$1), true)
		AND NOT EXISTS (SELECT 1 FROM simplefin_requests WHERE at > now() - interval '4 hours')`, simplefinItemID).Scan(&due)
	return due, err
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		var jevMerchants int
		if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM jev_suggestions`).Scan(&jevMerchants); err != nil {
			apiError(w, err)
			return
		}
		usage, err := a.jevUsage(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"simplefin_configured": a.simplefin.accessURL != "", "jev_enabled": a.jev != nil, "fund_lookups": os.Getenv("SEC_USER_AGENT") != "", "jev_merchants": jevMerchants, "jev_usage": usage})
	})
	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		if err := a.syncNow(r.Context()); err != nil {
			// Sync status is operator-facing and already exposed as items.last_error, so keep the detail here.
			log.Printf("sync: %v", err)
			jsonResponse(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/sync", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, a.simplefin.progress.get()) })
	mux.HandleFunc("GET /api/items", func(w http.ResponseWriter, r *http.Request) {
		rows, err := a.db.QueryContext(r.Context(), `SELECT i.id,COUNT(a.id),i.last_synced_at,i.last_error,(SELECT COUNT(*) FROM simplefin_requests WHERE at > now() - interval '24 hours') FROM items i LEFT JOIN accounts a ON a.item_id=i.id WHERE i.id='simplefin' GROUP BY i.id ORDER BY i.id`)
		if err != nil {
			apiError(w, err)
			return
		}
		result := make([]map[string]any, 0)
		for rows.Next() {
			var id, lastError string
			var count, requests int
			var lastSynced sql.NullString
			if err := rows.Scan(&id, &count, &lastSynced, &lastError, &requests); err != nil {
				rows.Close()
				apiError(w, err)
				return
			}
			var synced any
			if lastSynced.Valid {
				synced = lastSynced.String
			}
			result = append(result, map[string]any{"id": id, "institutions": []string{}, "accounts": count, "last_synced_at": synced, "last_error": lastError,
				"requests_24h": requests, "request_limit": simplefinDailyRequests})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			apiError(w, err)
			return
		}
		byID := make(map[string]map[string]any, len(result))
		for _, item := range result {
			byID[item["id"].(string)] = item
		}
		found, err := a.db.QueryContext(r.Context(), `SELECT DISTINCT item_id,institution FROM accounts WHERE institution != '' AND item_id='simplefin' ORDER BY institution`)
		if err != nil {
			apiError(w, err)
			return
		}
		for found.Next() {
			var id, name string
			if err := found.Scan(&id, &name); err != nil {
				found.Close()
				apiError(w, err)
				return
			}
			byID[id]["institutions"] = append(byID[id]["institutions"].([]string), name)
		}
		err = found.Err()
		found.Close()
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	mux.HandleFunc("GET /api/summary/monthly", func(w http.ResponseWriter, r *http.Request) {
		v, err := a.monthly(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, v)
	})
	mux.HandleFunc("GET /api/summary/categories", func(w http.ResponseWriter, r *http.Request) {
		month := r.URL.Query().Get("month")
		if month == "" {
			month = time.Now().Format("2006-01")
		}
		if !validPeriod(month) {
			jsonResponse(w, 400, map[string]string{"error": "month must be YYYY-MM or YYYY"})
			return
		}
		rows, err := a.db.QueryContext(r.Context(), `SELECT effective_category, -SUM(amount) FROM cashflow WHERE substr(date,1,$1)=$2 AND amount<0 GROUP BY effective_category ORDER BY 2 DESC`, len(month), month)
		queryRows(w, rows, err, func(rows *sql.Rows) (any, error) {
			var category string
			var amount int64
			err := rows.Scan(&category, &amount)
			return map[string]any{"category": category, "amount": amount}, err
		})
	})
	mux.HandleFunc("GET /api/accounts", func(w http.ResponseWriter, r *http.Request) {
		rows, err := a.db.QueryContext(r.Context(), `SELECT a.institution,a.id,a.name,a.mask,a.type,a.user_type,a.guess_confident,a.subtype,a.current,a.available,a.currency,a.item_id FROM accounts a ORDER BY a.institution,a.name`)
		queryRows(w, rows, err, func(rows *sql.Rows) (any, error) {
			var institution, id, name, mask, typ, userType, subtype, currency, itemID string
			var guessConfident bool
			var current, available sql.NullInt64
			err := rows.Scan(&institution, &id, &name, &mask, &typ, &userType, &guessConfident, &subtype, &current, &available, &currency, &itemID)
			var c, av any
			if current.Valid {
				c = current.Int64
			}
			if available.Valid {
				av = available.Int64
			}
			return map[string]any{"institution": institution, "id": id, "name": name, "mask": mask, "type": typ, "type_source": typeSource(userType), "type_confident": userType != "" || guessConfident, "subtype": subtype, "current": c, "available": av, "currency": currency, "item_id": itemID}, err
		})
	})
	mux.HandleFunc("GET /api/transactions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, offset := 100, 0
		if s := q.Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 1000 {
				jsonResponse(w, 400, map[string]string{"error": "limit must be 1–1000"})
				return
			}
			limit = n
		}
		if s := q.Get("offset"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				jsonResponse(w, 400, map[string]string{"error": "offset must be 0 or more"})
				return
			}
			offset = n
		}
		if m := q.Get("month"); m != "" && !validPeriod(m) {
			jsonResponse(w, 400, map[string]string{"error": "month must be YYYY-MM or YYYY"})
			return
		}
		if f := q.Get("flow"); f != "" && f != "in" && f != "out" {
			jsonResponse(w, 400, map[string]string{"error": "flow must be in or out"})
			return
		}
		// Filtering by category or flow (or ?uncategorized=1, the inbox) reads the cashflow view, so
		// transfers and pending rows drop out and totals match the summaries.
		source := "transactions"
		where, args := []string{"true"}, []any{}
		arg := func(v any) string { // appends v and returns its $n placeholder
			args = append(args, v)
			return "$" + strconv.Itoa(len(args))
		}
		if q.Get("uncategorized") == "1" {
			source = "cashflow"
			where = append(where, "t.effective_category=''")
		}
		if q.Has("category") { // category= (empty) means uncategorized
			source = "cashflow"
			where = append(where, "t.effective_category="+arg(q.Get("category")))
		}
		switch q.Get("flow") {
		case "in":
			source = "cashflow"
			where = append(where, "t.amount>0")
		case "out":
			source = "cashflow"
			where = append(where, "t.amount<0")
		}
		if m := q.Get("month"); m != "" {
			where = append(where, "substr(t.date,1,"+arg(len(m))+")="+arg(m))
		}
		if id := q.Get("account_id"); id != "" {
			where = append(where, "t.account_id="+arg(id))
		}
		if s := strings.TrimSpace(q.Get("q")); s != "" {
			p := "lower(" + arg(s) + "::text)"
			where = append(where, "(strpos(lower("+displayMerchantSQL+"),"+p+")>0 OR strpos(lower(t.merchant),"+p+")>0 OR strpos(lower(t.name),"+p+")>0)")
		}
		clause := " WHERE " + strings.Join(where, " AND ")
		var total int
		if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM (SELECT * FROM `+source+`) t`+clause, args...).Scan(&total); err != nil {
			apiError(w, err)
			return
		}
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		limitArg, offsetArg := arg(limit), arg(offset) // after the count query, which takes only the filter args
		rows, err := a.db.QueryContext(r.Context(), `SELECT t.id,t.date,t.amount,t.name,`+displayMerchantSQL+`,t.effective_category,t.pending,t.account_id,a.name,a.institution,
			t.transfer_id IS NOT NULL,t.user_category!='',a.type
			FROM (SELECT * FROM `+source+`) t JOIN accounts a ON a.id=t.account_id`+clause+` ORDER BY t.date DESC,t.id DESC LIMIT `+limitArg+` OFFSET `+offsetArg, args...)
		queryRows(w, rows, err, func(rows *sql.Rows) (any, error) {
			var id, date, name, merchant, category, accountID, account, institution string
			var amount int64
			var pending, transfer, manual bool
			var accountType string
			err := rows.Scan(&id, &date, &amount, &name, &merchant, &category, &pending, &accountID, &account, &institution, &transfer, &manual, &accountType)
			return map[string]any{"id": id, "date": date, "amount": amount, "name": name, "merchant": merchant, "category": category, "pending": pending, "account_id": accountID, "account": account, "institution": institution, "transfer": transfer, "manual": manual,
				"account_type": accountType}, err
		})
	})
	// Income by source (merchant, else name) for a month or year, largest first; the UI keeps the top few.
	mux.HandleFunc("GET /api/summary/income", func(w http.ResponseWriter, r *http.Request) {
		period := r.URL.Query().Get("month")
		if !validPeriod(period) {
			jsonResponse(w, 400, map[string]string{"error": "month must be YYYY-MM or YYYY"})
			return
		}
		rows, err := a.db.QueryContext(r.Context(), `SELECT COALESCE(NULLIF(merchant,''),name),SUM(amount) FROM cashflow WHERE amount>0 AND substr(date,1,$1)=$2 GROUP BY 1 ORDER BY 2 DESC,1`, len(period), period)
		queryRows(w, rows, err, func(rows *sql.Rows) (any, error) {
			var source string
			var amount int64
			err := rows.Scan(&source, &amount)
			return map[string]any{"source": source, "amount": amount}, err
		})
	})
	mux.HandleFunc("GET /api/networth", func(w http.ResponseWriter, r *http.Request) {
		rows, err := a.db.QueryContext(r.Context(), `SELECT date,SUM(current) FROM balances GROUP BY date ORDER BY date`)
		queryRows(w, rows, err, func(rows *sql.Rows) (any, error) {
			var date string
			var total int64
			err := rows.Scan(&date, &total)
			return map[string]any{"date": date, "total": total}, err
		})
	})
	a.categoryRoutes(mux)
	a.accountRoutes(mux)
	a.investmentRoutes(mux)
	a.merchantRoutes(mux)
	a.backupRoutes(mux)
	a.recurringRoutes(mux)
	a.alertRoutes(mux)
	dist, _ := fs.Sub(site, "web/dist")
	mux.Handle("/", http.FileServerFS(dist))
	return mux
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "simplefin-claim" {
		os.Exit(runSimplefinClaim(os.Stdin, os.Stdout, os.Stderr, simplefinClient{}))
	}
	sf, err := simplefinFromEnv(os.Getenv("SIMPLEFIN_ACCESS_URL"))
	if err != nil {
		log.Fatal(err)
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required (e.g. postgres://user:pass@localhost:5432/money?sslmode=disable); `just pg` starts a local one")
	}
	db, err := openDB(databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("database: %s", describeDB(databaseURL))
	defer db.Close()
	if restoreCLI(db, os.Args[1:]) {
		return
	}
	if ran, err := importMonarchCLI(context.Background(), db, os.Args[1:], os.Stdout); ran {
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	a := &app{db: db, simplefin: sf, jev: newJevFromEnv()}
	if a.jev != nil {
		a.jev.usage = db
	}
	if sf.accessURL == "" {
		log.Print("SimpleFIN is not configured; set SIMPLEFIN_ACCESS_URL to sync accounts")
	} else if err := a.ensureSimplefinItem(); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go a.backupLoop(ctx)
	go a.propertyLoop(ctx)
	// Checks hourly against the database, so restarts never reset or repeat the schedule. SimpleFIN asks for a
	// random minute past the hour (the top of the hour is busiest), so the minute is picked once per start.
	go func() {
		if a.simplefin.accessURL == "" {
			return
		}
		minute := time.Duration(rand.IntN(50)+5) * time.Minute
		for {
			now := time.Now()
			next := now.Truncate(time.Hour).Add(minute)
			if !next.After(now) {
				next = next.Add(time.Hour)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			if due, err := syncDue(ctx, a.db); err != nil {
				log.Printf("background sync: %v", err)
			} else if due {
				if err := a.syncNow(ctx); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("background sync: %v", err)
				}
			}
		}
	}()
	server := &http.Server{Addr: env("ADDR", ":8080"), Handler: security(mustAuth(ctx, a.routes())),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		// POST /api/sync runs every item sync inline (30s per upstream request, several windows per item).
		WriteTimeout: 10 * time.Minute}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
