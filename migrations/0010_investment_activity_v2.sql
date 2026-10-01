-- investment_activity, revised after the Monarch import:
--   * SHARESALE counts as a contribution: vests and ESPP purchases arrive as $0 share deposits that the balance never
--     reflects, and are sold within days, so the sale is when their value enters (the journal out and the transfer
--     to checking then take it back out);
--   * Roth conversions move money between own accounts: contributions, "incoming" positive and "outgoing" negative
--     whatever sign the export used (Monarch's are reversed).
DROP VIEW investment_activity;
CREATE VIEW investment_activity AS
SELECT t.id, t.account_id, t.date,
  CASE WHEN t.name ~* 'conversion \(incoming\)' THEN abs(t.amount) WHEN t.name ~* 'conversion \(outgoing\)' THEN -abs(t.amount) ELSE t.amount END AS amount,
  t.name, CASE
    WHEN t.transfer_id IS NOT NULL THEN 'contribution'
    WHEN t.name ~* '(contribution|funds received|withdrawal|restricted stock|stock purchase plan deposit|journal (to|frm|from)\M|sharesale|conversion \((incoming|outgoing)\))' THEN 'contribution'
    WHEN t.name ~* '(dividend|interest|bank int\M|capital gain)' THEN 'income'
    WHEN t.amount > 0 AND EXISTS (SELECT 1 FROM transactions o WHERE o.account_id = t.account_id AND o.date = t.date
      AND o.name = t.name AND o.amount = -t.amount AND o.id <> t.id) THEN 'income'
    ELSE 'internal' END AS kind
FROM transactions t JOIN accounts a ON a.id = t.account_id
WHERE NOT t.pending AND a.type IN ('investment', 'other');
