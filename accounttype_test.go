package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuessAccountType(t *testing.T) {
	for _, c := range []struct {
		institution, name string
		balance           int64
		typ               string
		confident         bool
	}{
		{"Chase", "Chase Sapphire Preferred", -120000, "credit", true},
		{"Chase", "Freedom Unlimited", 0, "credit", true},
		{"Capital One", "Venture Rewards", -5000, "credit", true},
		{"Capital One", "Quicksilver", 0, "credit", true},
		{"Citi", "Double Cash Card", -100, "credit", true},
		{"Bank of America", "Customized Cash Rewards Visa", 0, "credit", true},
		{"American Express", "Gold", -9900, "credit", true},
		{"Discover", "it Card", -1, "credit", true},
		{"Wells Fargo", "Everyday Checking", 250000, "depository", true},
		{"Ally", "Online Savings", 1000000, "depository", true},
		{"Fidelity", "Cash Management Money Market", 5000, "depository", true},
		{"Local CU", "Share Savings", 500, "depository", true},
		{"Bank", "12-Month CD", 1000000, "depository", true},
		{"HSA Bank", "HSA", 120000, "depository", true},
		{"Fidelity", "Health Savings HSA Investment", 800000, "investment", true},
		{"Vanguard", "Brokerage Account", 5000000, "investment", true},
		{"Fidelity", "Individual", 4200000, "investment", true},
		{"Fidelity", "Joint Brokerage", 100, "investment", true},
		{"Employer", "401(k)", 9000000, "investment", true},
		{"Employer", "Company 401k Plan", 9000000, "investment", true},
		{"School", "403(b)", 100, "investment", true},
		{"Gov", "457 Deferred Comp", 100, "investment", true},
		{"Schwab", "Roth IRA", 100, "investment", true},
		{"Vanguard", "Rollover IRA", 100, "investment", true},
		{"Schwab", "Investment Account", 100, "investment", true},
		{"Family", "Family Trust", 100, "investment", true},
		{"Former Employer", "Pension", 100, "investment", true},
		{"Rocket", "30-Year Mortgage", -30000000, "loan", true},
		{"Bank", "HELOC", -1000000, "loan", true},
		{"Ally", "Auto Loan", -1500000, "loan", true},
		{"Navient", "Student Loan", -2000000, "loan", true},
		{"Bank", "Personal Line of Credit", -100, "loan", true},
		// Ambiguous: rule order decides (loan, then investment, then credit, then cash).
		{"Bank", "Credit Card Savings Loan", 0, "loan", true},
		{"Bank", "Line of Credit Card", -1, "loan", true},
		{"Bank", "Roth IRA Money Market", 1, "investment", true},
		{"Bank", "Overdrawn Checking", -500, "depository", true},
		// Institution helps only when the name says nothing.
		{"American Express", "Blue", -100, "credit", true},
		// Fallbacks.
		{"Mystery Bank", "Account 1234", -5000, "credit", false},
		{"Mystery Bank", "Account 1234", 5000, "other", false},
		{"", "", 0, "other", false},
	} {
		typ, confident := guessAccountType(c.institution, c.name, c.balance)
		if typ != c.typ || confident != c.confident {
			t.Errorf("%q / %q / %d = %q,%v; want %q,%v", c.institution, c.name, c.balance, typ, confident, c.typ, c.confident)
		}
	}
}

type acctState struct {
	Type, Source string
	Confident    bool
}

func (a *app) acctState(t *testing.T, id string) acctState {
	t.Helper()
	var list []struct {
		ID            string `json:"id"`
		Type          string `json:"type"`
		TypeSource    string `json:"type_source"`
		TypeConfident bool   `json:"type_confident"`
	}
	a.call(t, "GET", "/api/accounts", nil, 200, &list)
	for _, x := range list {
		if x.ID == id {
			return acctState{x.Type, x.TypeSource, x.TypeConfident}
		}
	}
	t.Fatalf("account %s missing", id)
	return acctState{}
}

func TestAccountTypeGuessOverrideAndClear(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"accounts":[{"org":{"name":"Test Bank"},"id":"visa","name":"Sapphire","balance":"-10.00"},{"org":{"name":"Test Bank"},"id":"mystery","name":"Acct 9","balance":"5.00"}]}`)
	}))
	defer server.Close()
	a := categoryApp(t)
	a.simplefin = simplefinClient{client: server.Client()}
	accessURL := strings.Replace(server.URL, "https://", "https://user:pass@", 1)
	sync := func() {
		t.Helper()
		if _, err := a.simplefin.syncItem(context.Background(), a.db, "item", accessURL); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	if got := a.acctState(t, "sf:visa"); got != (acctState{"credit", "guess", true}) {
		t.Errorf("visa = %+v", got)
	}
	if got := a.acctState(t, "sf:mystery"); got != (acctState{"other", "guess", false}) {
		t.Errorf("mystery = %+v", got)
	}

	a.call(t, "PATCH", "/api/accounts/sf:mystery", map[string]string{"type": "investment"}, 200, nil)
	want := acctState{"investment", "user", true}
	if got := a.acctState(t, "sf:mystery"); got != want {
		t.Errorf("after override = %+v", got)
	}
	sync() // sync must never overwrite the user's choice
	if got := a.acctState(t, "sf:mystery"); got != want {
		t.Errorf("override lost on re-sync: %+v", got)
	}
	a.call(t, "PATCH", "/api/accounts/sf:mystery", map[string]string{"type": ""}, 200, nil)
	if got := a.acctState(t, "sf:mystery"); got != (acctState{"other", "guess", false}) {
		t.Errorf("after clearing = %+v", got)
	}

	a.call(t, "PATCH", "/api/accounts/sf:mystery", map[string]string{"type": "bogus"}, 400, nil)
	a.call(t, "PATCH", "/api/accounts/sf:mystery", map[string]string{}, 400, nil)
	a.call(t, "PATCH", "/api/accounts/nope", map[string]string{"type": "loan"}, 404, nil)
}
