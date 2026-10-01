package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// TypeSafe Jev is the optional "phone a friend" fallback: local sources answer first and Jev only sees what they could not.
// Off unless JEV_API_KEY is set. API: POST /v1/systemone, one shared "state" plus keyed "choice" questions, answers under "answers".
// Sent for merchants: display name, up to 3 raw bank descriptions, typical amount, income/spending. Sent for accounts:
// institution, account name, balance sign. Nothing else (no ids, balances, dates or transactions).

const (
	jevDefaultURL = "https://api.typesafe.ai/v1/systemone"
	// The docs state no per-request limit on questions (only 255 options per question), so batches are kept modest.
	jevBatch        = 20
	jevMaxResponse  = 4 << 20
	jevDefaultModel = "jev-latest"
)

type jevClient struct {
	key, url, model string
	minConfidence   float64
	home            string // JEV_HOME_LOCATION, e.g. "Denver, Colorado"; '' = not sent
	http            *http.Client
	usage           *sql.DB // where each request's token counts go; nil = not recorded (tests)
	// Dollars per million tokens: TypeSafe's $0.042 input and free output (2026-09-30), overridable with
	// JEV_PRICE_INPUT_PER_MTOK / JEV_PRICE_OUTPUT_PER_MTOK.
	priceIn, priceOut float64
}

// newJevFromEnv returns nil (Jev off) unless JEV_API_KEY is set.
func newJevFromEnv() *jevClient {
	key := os.Getenv("JEV_API_KEY")
	if key == "" {
		return nil
	}
	min, err := strconv.ParseFloat(os.Getenv("JEV_MIN_CONFIDENCE"), 64)
	if err != nil || min < 0 || min > 1 {
		min = 0.5
	}
	return &jevClient{key: key, url: env("JEV_URL", jevDefaultURL), model: env("JEV_MODEL", jevDefaultModel), minConfidence: min, home: strings.TrimSpace(os.Getenv("JEV_HOME_LOCATION")),
		priceIn: envPrice("JEV_PRICE_INPUT_PER_MTOK", 0.042), priceOut: envPrice("JEV_PRICE_OUTPUT_PER_MTOK", 0), http: &http.Client{Timeout: 30 * time.Second}}
}

func envPrice(name string, fallback float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err != nil || v < 0 {
		return fallback
	}
	return v
}

type jevQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type jevAnswer struct {
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

func (j *jevClient) redact(err error) error {
	return errors.New(strings.ReplaceAll(err.Error(), j.key, "[redacted]"))
}

// ask sends one batched request. Errors never contain the API key. No retry on 429/529: callers fall through and ask again later.
func (j *jevClient) ask(ctx context.Context, state any, questions map[string]jevQuestion) (map[string]jevAnswer, error) {
	body, err := json.Marshal(map[string]any{"model": j.model, "state": state, "questions": questions})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.url, bytes.NewReader(body))
	if err != nil {
		return nil, j.redact(err)
	}
	req.Header.Set("Authorization", "Bearer "+j.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := j.http.Do(req)
	if err != nil {
		return nil, j.redact(fmt.Errorf("jev request failed: %w", err))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jev returned HTTP %d", res.StatusCode)
	}
	var out struct {
		Answers map[string]jevAnswer `json:"answers"`
		Usage   struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, jevMaxResponse)).Decode(&out); err != nil {
		return nil, j.redact(fmt.Errorf("invalid jev response: %w", err))
	}
	if j.usage != nil {
		if _, err := j.usage.ExecContext(ctx, `INSERT INTO jev_usage(questions,input_tokens,output_tokens) VALUES($1,$2,$3)`,
			len(questions), out.Usage.InputTokens, out.Usage.OutputTokens); err != nil {
			log.Printf("jev usage: %v", err) // bookkeeping only; the answers still count
		}
	}
	return out.Answers, nil
}

// choices asks one batch of questions and keeps only answers whose choice is one of the criteria. Callers apply
// minConfidence: merchants keep weak answers as marked suggestions, account types drop them.
func (j *jevClient) choices(ctx context.Context, state map[string]any, qs map[string]jevQuestion) (map[string]jevAnswer, error) {
	answers, err := j.ask(ctx, state, qs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]jevAnswer)
	for id, a := range answers {
		if q, ok := qs[id]; ok {
			if _, valid := q.Criteria[a.Choice]; valid {
				out[id] = a
			}
		}
	}
	return out, nil
}

// jevSource is a SuggestionSource backed by Jev. Every answer, including "no confident answer", is saved in
// jev_suggestions and reused, so a merchant is asked once until the user asks Jev again.
type jevSource struct {
	j  *jevClient
	db *sql.DB
}

func (jevSource) Name() string { return "jev" }

// jevUsage totals recorded Jev requests for the last 30 days and all time, with the estimated cost in dollars.
func (a *app) jevUsage(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range []struct{ key, since string }{{"last_30_days", "now() - interval '30 days'"}, {"all_time", "'-infinity'"}} {
		var requests, in, outTok int64
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0) FROM jev_usage WHERE at > `+p.since).Scan(&requests, &in, &outTok); err != nil {
			return nil, err
		}
		u := map[string]any{"requests": requests, "input_tokens": in, "output_tokens": outTok}
		if a.jev != nil {
			u["cost"] = (float64(in)*a.jev.priceIn + float64(outTok)*a.jev.priceOut) / 1e6
		}
		out[p.key] = u
	}
	return out, nil
}

// jevMerchantHints is the shared guidance in every merchant question: what each field means, and the card-processor
// prefixes that hide what a merchant is.
const jevMerchantHints = "Use its name, bank descriptions, typical amount, kind (income or spending), how many charges it has, " +
	"and typical_days_between_charges when present (about 30 or 365 at a steady amount suggests a subscription or bill). " +
	"If household.examples is present, those are merchants this household already categorized: follow the same conventions for similar merchants. " +
	"If household.home is present, it is where the household lives: national parks, resorts and places far from home usually mean Travel. " +
	"Bank description prefixes: TST* is Toast and PAR* is PAR point of sale, almost always restaurants, bars or cafes; " +
	"SQ * is Square, a small local business such as a cafe, food truck, market stall or shop; " +
	"SP  is Shopify, an online store (judge by the store name); PAYPAL * and VENMO wrap the real payee; " +
	"AMZN or AMAZON MKTPL is Amazon; APPLE.COM/BILL and GOOGLE * are app store or subscription charges; " +
	"PADDLE.NET* and FS * resell software subscriptions; DD * or DOORDASH and UBER EATS are food delivery (Dining), while UBER TRIP or LYFT is rideshare."

// household is shared context for every batch: the home location if set, and up to 3 of the most-used merchants
// per category that the user already categorized (names and categories only).
func (s jevSource) household(ctx context.Context) (map[string]any, error) {
	h := map[string]any{}
	if s.j.home != "" {
		h["home"] = s.j.home
	}
	rows, err := s.db.QueryContext(ctx, `SELECT display_name, cat FROM (
			SELECT mr.display_name, c.effective_category cat, ROW_NUMBER() OVER (PARTITION BY c.effective_category ORDER BY COUNT(*) DESC, mr.display_name) rk
			FROM cashflow c JOIN merchant_roots mr ON mr.key=c.merchant_key WHERE c.effective_category<>'' GROUP BY mr.display_name, c.effective_category) u
		WHERE rk<=3 ORDER BY cat, rk LIMIT 60`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var examples []map[string]string
	for rows.Next() {
		var name, cat string
		if err := rows.Scan(&name, &cat); err != nil {
			return nil, err
		}
		examples = append(examples, map[string]string{"merchant": name, "category": cat})
	}
	if len(examples) > 0 {
		h["examples"] = examples
	}
	return h, rows.Err()
}

// reason marks answers under JEV_MIN_CONFIDENCE as a guess; they are still offered, never applied on their own.
func (s jevSource) reason(confidence float64) string {
	if confidence < s.j.minConfidence {
		return "Jev's best guess, low confidence"
	}
	return "Suggested by Jev"
}

// Suggest asks in batches. A failing batch stops asking; answers from earlier batches are kept, and
// only a failure with nothing answered is returned as an error (the caller logs it and falls through).
func (s jevSource) Suggest(ctx context.Context, samples []MerchantSample, categories []CategoryChoice) (map[string]Suggestion, error) {
	criteria := make(map[string]string, len(categories))
	for _, c := range categories {
		criteria[c.Name] = c.Description
	}
	out := make(map[string]Suggestion)
	var ask []MerchantSample
	for _, m := range samples {
		var g Suggestion
		var probs []byte
		err := s.db.QueryRowContext(ctx, `SELECT category,confidence,probabilities FROM jev_suggestions WHERE merchant_key=$1`, m.Key).Scan(&g.Category, &g.Confidence, &probs)
		if errors.Is(err, sql.ErrNoRows) {
			ask = append(ask, m)
			continue
		}
		if err != nil {
			return nil, err
		}
		if g.Category != "" {
			_ = json.Unmarshal(probs, &g.Probabilities) // NULL or bad JSON: just no probabilities
			g.Source, g.Reason = "jev", s.reason(g.Confidence)
			out[m.Key] = g
		}
	}
	samples = ask
	if len(samples) == 0 {
		return out, nil
	}
	household, err := s.household(ctx)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(samples); start += jevBatch {
		batch := samples[start:min(start+jevBatch, len(samples))]
		merchants := make(map[string]any, len(batch))
		qs := make(map[string]jevQuestion, len(batch))
		keys := make(map[string]string, len(batch))
		for i, m := range batch {
			id := "q" + strconv.Itoa(i)
			kind := "spending"
			if m.TypicalAmount > 0 {
				kind = "income"
			}
			merchant := map[string]any{"name": m.Display, "bank_descriptions": m.RawNames, "typical_amount": float64(m.TypicalAmount) / 100, "kind": kind, "charges": m.Charges}
			if m.TypicalGap > 0 {
				merchant["typical_days_between_charges"] = m.TypicalGap
			}
			merchants[id] = merchant
			qs[id] = jevQuestion{"choice", "Which category best fits merchant " + id + " in the state? " + jevMerchantHints, criteria}
			keys[id] = m.Key
		}
		state := map[string]any{"merchants": merchants}
		if len(household) > 0 {
			state["household"] = household
		}
		got, err := s.j.choices(ctx, state, qs)
		if err != nil {
			if len(out) == 0 {
				return nil, err
			}
			log.Printf("suggestion source jev: %v (keeping %d earlier answers)", err, len(out))
			return out, nil
		}
		for id, key := range keys {
			a, ok := got[id]
			var probs []byte
			if ok {
				out[key] = Suggestion{Category: a.Choice, Confidence: a.Confidence, Probabilities: a.Probabilities, Source: "jev", Reason: s.reason(a.Confidence)}
				if len(a.Probabilities) > 0 {
					probs, _ = json.Marshal(a.Probabilities)
				}
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO jev_suggestions(merchant_key,category,confidence,probabilities) VALUES($1,$2,$3,$4)
				ON CONFLICT(merchant_key) DO UPDATE SET category=excluded.category,confidence=excluded.confidence,probabilities=excluded.probabilities,asked_at=now()`,
				key, a.Choice, a.Confidence, probs); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

var accountTypeDescriptions = map[string]string{
	"depository": "Checking, savings, money market, or CD account",
	"credit":     "Credit card or other revolving credit",
	"investment": "Brokerage, retirement (IRA, 401k), HSA investment, or similar",
	"loan":       "Mortgage, auto, student, or personal loan",
	"other":      "Anything that fits none of the above",
}

// jevKept is SQL: true when the row's non-confident guess should stay the stored Jev answer (name unchanged since it was asked).
func jevKept(name, localConfident string) string {
	return "(NOT " + localConfident + " AND accounts.jev_type!='' AND accounts.jev_asked_name=" + name + ")"
}

func guessedTypeSQL(name, local, localConfident string) string {
	return "CASE WHEN " + jevKept(name, localConfident) + " THEN accounts.jev_type ELSE " + local + " END"
}

// jevAccountTypes asks Jev, in one batch, about accounts with no user type and a non-confident guess whose name was
// not asked about yet. Answers at or above the threshold become the guess (never user_type). Failures are only logged.
func (a *app) jevAccountTypes(ctx context.Context) {
	if a.jev == nil {
		return
	}
	if err := a.askJevAccountTypes(ctx); err != nil {
		log.Printf("jev account types: %v", a.jev.redact(err))
	}
}

func (a *app) askJevAccountTypes(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,institution,name,current FROM accounts WHERE user_type='' AND NOT guess_confident AND jev_asked_name!=name ORDER BY id LIMIT $1`, jevBatch)
	if err != nil {
		return err
	}
	defer rows.Close()
	accounts := make(map[string]any)
	qs := make(map[string]jevQuestion)
	type asked struct{ id, name string }
	byQ := make(map[string]asked)
	for rows.Next() {
		var id, institution, name string
		var current *int64
		if err := rows.Scan(&id, &institution, &name, &current); err != nil {
			return err
		}
		q := "q" + strconv.Itoa(len(byQ))
		sign := "unknown"
		if current != nil {
			sign = "zero"
			if *current > 0 {
				sign = "positive"
			} else if *current < 0 {
				sign = "negative"
			}
		}
		accounts[q] = map[string]string{"institution": institution, "name": name, "balance_sign": sign}
		qs[q] = jevQuestion{"choice", "What type of financial account is " + q + " in the state? Liabilities usually have a negative balance.", accountTypeDescriptions}
		byQ[q] = asked{id, name}
	}
	if err := rows.Err(); err != nil || len(byQ) == 0 {
		return err
	}
	rows.Close()
	answers, err := a.jev.choices(ctx, map[string]any{"accounts": accounts}, qs)
	if err != nil {
		return err
	}
	for q, ac := range byQ { // low-confidence or missing answers are recorded too, so the same name is not re-asked
		typ := answers[q].Choice
		if answers[q].Confidence < a.jev.minConfidence {
			typ = ""
		}
		if _, err := a.db.ExecContext(ctx, `UPDATE accounts SET jev_asked_name=$1,jev_type=$2,
			guessed_type=CASE WHEN $2::text!='' THEN $2::text ELSE guessed_type END,guess_confident=guess_confident OR $2::text!=''
			WHERE id=$3 AND name=$1 AND user_type=''`, ac.name, typ, ac.id); err != nil {
			return err
		}
	}
	return nil
}
