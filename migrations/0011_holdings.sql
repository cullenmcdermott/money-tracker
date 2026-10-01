-- Brokerage holdings as SimpleFIN Bridge sends them (outside the protocol), kept raw until a feature needs typed columns.
-- Each sync replaces an account's rows, so this is the latest snapshot only.
CREATE TABLE holdings (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  raw JSONB NOT NULL,
  synced_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX holdings_account ON holdings(account_id);
