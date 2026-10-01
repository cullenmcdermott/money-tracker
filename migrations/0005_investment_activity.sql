-- What each transaction inside an investment (or other) account is, computed on read so account type changes apply at once:
--   contribution: money or shares arriving from outside the account (matched transfers, payroll contributions, funds
--                 received, vests and ESPP deposits, journals between own accounts) or leaving it (withdrawals; negative);
--   income:       dividends and interest, including the positive half of a reinvested dividend, which SimpleFIN shows as
--                 two rows with the fund's name on the same day (+x, -x);
--   internal:     everything else: buys, sells, reinvestments, sweeps, exchanges. They move value inside the account.
-- Vests and ESPP deposits carry $0 in the data; their value is estimated from the balance history where it is used.
CREATE VIEW investment_activity AS
SELECT t.id, t.account_id, t.date, t.amount, t.name, CASE
    WHEN t.transfer_id IS NOT NULL THEN 'contribution'
    WHEN t.name ~* '(contribution|funds received|withdrawal|restricted stock|stock purchase plan deposit|journal (to|frm|from)\M)' THEN 'contribution'
    WHEN t.name ~* '(dividend|interest|bank int\M|capital gain)' THEN 'income'
    WHEN t.amount > 0 AND EXISTS (SELECT 1 FROM transactions o WHERE o.account_id = t.account_id AND o.date = t.date
      AND o.name = t.name AND o.amount = -t.amount AND o.id <> t.id) THEN 'income'
    ELSE 'internal' END AS kind
FROM transactions t JOIN accounts a ON a.id = t.account_id
WHERE NOT t.pending AND a.type IN ('investment', 'other');
