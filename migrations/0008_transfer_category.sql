-- The Transfer category marks money moved between own accounts whose other side is not tracked (history imported
-- from Monarch, or an account SimpleFIN does not have): like a matched pair, it never counts as income or spending.
DROP VIEW cashflow;
CREATE VIEW cashflow AS SELECT t.* FROM transactions t JOIN accounts a ON a.id = t.account_id
  WHERE NOT t.pending AND t.transfer_id IS NULL AND t.effective_category <> 'Transfer' AND a.type IN ('depository', 'credit');
