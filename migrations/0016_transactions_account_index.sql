-- Per-account lookups: the investments page and plan (investment_activity's own-account EXISTS), sync's per-account
-- pending cleanup, alerts, and deleting an account all filter transactions by account and date.
CREATE INDEX transactions_account_date ON transactions(account_id, date);
