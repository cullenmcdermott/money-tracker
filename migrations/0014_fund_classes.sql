-- How each held symbol splits across asset classes, in basis points (10000 = 100%). source: owner (set in the
-- app, never expires), sec (from the fund's latest N-PORT filing, as_of = filing date, looked up again after
-- 120 days) or stock (an operating company's shares).
CREATE TABLE fund_classes (
  symbol TEXT PRIMARY KEY,
  us_stock INT NOT NULL DEFAULT 0,
  intl_stock INT NOT NULL DEFAULT 0,
  bonds INT NOT NULL DEFAULT 0,
  cash INT NOT NULL DEFAULT 0,
  other INT NOT NULL DEFAULT 0,
  source TEXT NOT NULL CHECK (source IN ('owner','sec','stock')),
  as_of TEXT NOT NULL DEFAULT '',
  saved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (us_stock>=0 AND intl_stock>=0 AND bonds>=0 AND cash>=0 AND other>=0 AND us_stock+intl_stock+bonds+cash+other=10000)
);
