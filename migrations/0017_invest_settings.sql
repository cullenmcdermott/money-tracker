-- Invest a deposit: per investment account, how to pick the target balance
-- between its funds (investDoc in invest.go), as one JSON document.
CREATE TABLE invest_settings (
  account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  doc JSONB NOT NULL
);
