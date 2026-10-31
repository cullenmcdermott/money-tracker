package main

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Trimmed-down pdftotext -layout output of each layout, with invented accounts and amounts.
const (
	chaseText = `
    Account Number: XXXX XXXX XXXX 1111
    Previous Balance                                                             $1,000.00
    New Balance                                                                    $955.30
    Opening/Closing Date                                         12/15/22 - 01/14/23
 ACCOUNT ACTIVITY
 PAYMENTS AND OTHER CREDITS
  01/10                        AUTOMATIC PAYMENT - THANK YOU                                    -1,000.00
 PURCHASE
  12/20                        CORNER CAFE ANYTOWN ST                                                5.75
                         Order Number  111-2222222-3333333
     01/02               POST OFFICE ANYTOWN ST                                                        .70
  01/05                        GROCER #12 ANYTOWN ST                                               948.85
                                                                   2023 Totals Year-to-Date
  01/14                        NOT ACTIVITY                                                           9.99
`
	usaaCardText = `
   USAA CREDIT CARD PAYMENTS
                  Account Number 4000 XXXX XXXX 2222
                                                                         Statement Closing Date      03/17/22
   Previous Balance                                   $10.00           New Balance       $44.62
    Payments and Credits
   Trans Date Post Date               Reference Number                          Description          Amount
      03/01     03/01                 F330900CV00CHGDDA                         AUTOMATIC PAYMENT - THANK YOU   $10.00-
      02/26     02/27                 2444500B7RLDAHH21                         SKI LODGE ANYTOWN ST            $34.62
      03/17     03/17                                                           Interest Charge on Purchases     $0.00
      03/17     03/17                                                           GAS STATION ANYTOWN ST           $10.00
Interest Charge Calculation
      03/17     03/17                                                           NOT ACTIVITY                     $99.00
`
	usaaBankText = `
USAA Federal Savings Bank
                                                                                          for Account Number: 0000003333
                                                                               Statement Period: 12/20/2023 to 01/19/2024
Beginning Balance                                                              $1,000.00
Ending Balance                                                                 $3,450.00
Transactions
 Date     Description                                                            Debits           Credits        Balance
 12/20     Beginning Balance                                                                                   $1,000.00
 12/22     ACH DEP 122223                                                                        $2,500.00      $3,500.00
          EXAMPLE CORP DIRECT DEP
          ***********AB12
 01/03     POS DEBIT 010324                                                       $50.00                        $3,450.00
          GROCER #12 ANYTOWN ST
                                                                                              continued on next page>>>
 01/19     Ending Balance                                                -                     -                    $3,450.00
`
	usaaOldText = `
     ACCOUNT NUMBER                                                  ACCOUNT TYPE                                              STATEMENT PERIOD
      00000-0444-4                                       USAA CLASSIC CHECKING                                               1 2 / 2 0 / 1 9 - 01/22/20
     BALANCE              DEBITS          TOTAL AMOUNT                 NO. OF
  LAST STATEMENT           PAID          OF DEBITS PAID                 DEP          OF DEPOSITS MADE                CHARGES               STATEMENT
      1,000.00                  3                   460.00                  2             2,000.21                    .00                     2,540.21
    DEPOSITS AND OTHER CREDITS
DATE..........AMOUNT.TRANSACTION DESCRIPTION
12/28       2,000.00 ACH CREDIT              122819
                     EXAMPLE CORP     PAYROLL     ***********39-0
01/22            .21 INTEREST PAID
    CHECKS
DATE..CHECK NO...........AMOUNT                                    DATE..CHECK NO...........AMOUNT
01/15      1206          210.00                                    01/16     1209*          200.00
    OTHER DEBITS
DATE..........AMOUNT.TRANSACTION DESCRIPTION
01/02          50.00 POS DEBIT
 ACCOUNT BALANCE SUMMARY
      12/20        1,000.00                               01/22        2,540.21
    CHECKS OUTSTANDING
`
	vanguardText = `
Statement overview                                                                                                                         $12,345.67
                                                                                                               Total value of all accounts as of January 31, 2024
Joint brokerage account—XXXX5555                                                                                                   Vanguard Personal Investor
Completed transactions
Settlement   Trade
date         date        Symbol        Name                                 Transaction type          Account type                         Quantity                 Price          & fees          Amount
12/29        12/29       -             ACH DIRECT WITHDRAWAL                Funds Received            -                                              -                  -                -      $1,000.00
01/17        01/16       VFIAX         VANGUARD                             Buy                       Cash                            0.6960         502.6700          0.00         -350.00
01/26        01/26       VTIAX         VANGUARD                             Dividend                  -                                    -           -             -          71.21
`
)

func TestParseStatements(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		want stmtAccount
	}{
		"chase": {chaseText, stmtAccount{Last4: "1111", End: "2023-01-14", Opening: -100000, Closing: -95530, Checked: true, Txs: []stmtTx{
			{"2023-01-10", 100000, "AUTOMATIC PAYMENT - THANK YOU"}, {"2022-12-20", -575, "CORNER CAFE ANYTOWN ST"},
			{"2023-01-02", -70, "POST OFFICE ANYTOWN ST"}, {"2023-01-05", -94885, "GROCER #12 ANYTOWN ST"}}}},
		"usaa card": {usaaCardText, stmtAccount{Last4: "2222", End: "2022-03-17", Opening: -1000, Closing: -4462, Checked: true, Txs: []stmtTx{
			{"2022-03-01", 1000, "AUTOMATIC PAYMENT - THANK YOU"}, {"2022-02-26", -3462, "SKI LODGE ANYTOWN ST"},
			{"2022-03-17", 0, "Interest Charge on Purchases"}, {"2022-03-17", -1000, "GAS STATION ANYTOWN ST"}}}},
		"usaa bank": {usaaBankText, stmtAccount{Last4: "3333", End: "2024-01-19", Opening: 100000, Closing: 345000, Checked: true, Txs: []stmtTx{
			{"2023-12-22", 250000, "ACH DEP 122223 EXAMPLE CORP DIRECT DEP"}, {"2024-01-03", -5000, "POS DEBIT 010324 GROCER #12 ANYTOWN ST"}}}},
		"usaa old": {usaaOldText, stmtAccount{Last4: "4444", End: "2020-01-22", Opening: 100000, Closing: 254021, Checked: true, Txs: []stmtTx{
			{"2019-12-28", 200000, "ACH CREDIT 122819 EXAMPLE CORP PAYROLL ***********39-0"}, {"2020-01-22", 21, "INTEREST PAID"},
			{"2020-01-15", -21000, "CHECK 1206"}, {"2020-01-16", -20000, "CHECK 1209"}, {"2020-01-02", -5000, "POS DEBIT"}}}},
		"vanguard": {vanguardText, stmtAccount{Last4: "5555", End: "2024-01-31", Closing: 1234567, Txs: []stmtTx{
			{"2023-12-29", 100000, "ACH DIRECT WITHDRAWAL Funds Received"}, {"2024-01-17", -35000, "VFIAX VANGUARD Buy"},
			{"2024-01-26", 7121, "VTIAX VANGUARD Dividend"}}}},
	} {
		got, err := parseStatement(tc.text)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(got) != 1 || !reflect.DeepEqual(got[0], tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, tc.want)
		}
	}
	if _, err := parseStatement("a letter, not a statement"); err == nil {
		t.Error("unknown layout parsed")
	}
}

func TestImportStatements(t *testing.T) {
	db, err := openTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO items(id) VALUES('simplefin');
		INSERT INTO accounts(id,item_id,name,guessed_type) VALUES('sf:chk','simplefin','EXAMPLE CHECKING (3333)','depository'),
			('sf:card','simplefin','REWARDS VISA (9999)','credit');
		INSERT INTO transactions(id,account_id,date,amount,name) VALUES
			('monarch:1','sf:chk','2024-01-04','-5000','GROCER'),        -- the statement's 01/03 row, a day apart
			('sf:1','sf:chk','2024-01-10','-100','LATER')`); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(usaaBankText, "$3,450.00\n", "$3,999.00\n", 1) // the ending balance no longer adds up
	docs := []stmtDoc{{"bank.pdf", usaaBankText}, {"bad.pdf", bad}, {"card.pdf", chaseText}}
	var out strings.Builder
	if err := importStatements(context.Background(), db, docs, "", false, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bank.pdf: sf:chk to 2024-01-19, 1 of 2 transactions new", "bad.pdf: SKIPPED", "card.pdf: SKIPPED, no account ending 1111"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	// The card's number changed: --account names it. Re-running the bank statement adds nothing.
	if err := importStatements(context.Background(), db, docs[:1], "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := importStatements(context.Background(), db, docs[2:], "9999", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	var n, cardRows int
	var bal int64
	db.QueryRow(`SELECT count(*) FROM transactions WHERE id LIKE 'stmt:%' AND account_id='sf:chk'`).Scan(&n)
	db.QueryRow(`SELECT count(*) FROM transactions WHERE id LIKE 'stmt:%' AND account_id='sf:card'`).Scan(&cardRows)
	db.QueryRow(`SELECT current FROM balances WHERE account_id='sf:card' AND date='2023-01-14'`).Scan(&bal)
	if n != 1 || cardRows != 4 || bal != -95530 {
		t.Errorf("checking rows %d, card rows %d, card balance %d; want 1, 4, -95530", n, cardRows, bal)
	}
}
