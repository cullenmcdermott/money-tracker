-- Income and spending come only from checking/savings (depository) and credit card accounts. Activity inside
-- investment, loan and other accounts (fund purchases, vests, dividends, loan principal) counts toward net worth,
-- not cash flow. Money moved between accounts stays out as before (matched transfer pairs).
DROP VIEW cashflow;
CREATE VIEW cashflow AS SELECT t.* FROM transactions t JOIN accounts a ON a.id = t.account_id
  WHERE NOT t.pending AND t.transfer_id IS NULL AND a.type IN ('depository', 'credit');
