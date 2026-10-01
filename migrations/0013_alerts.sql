-- Charges worth a look, found after each sync (alerts.go). UNIQUE(kind,key) makes detection idempotent, so a
-- dismissed alert never comes back. key is the transaction id for charge alerts and YYYY-MM:category for pace
-- alerts; it never holds a number that moves daily.
CREATE TABLE alerts (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind TEXT NOT NULL,
  key TEXT NOT NULL,
  transaction_id TEXT REFERENCES transactions(id) ON DELETE CASCADE,
  related_transaction_id TEXT REFERENCES transactions(id) ON DELETE SET NULL, -- card testing: the small test charge
  reasons JSONB NOT NULL DEFAULT '[]',
  jev_choice TEXT NOT NULL DEFAULT '', -- '' = not triaged (Jev off, failed, or not asked yet)
  jev_confidence REAL NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  dismissed_at TIMESTAMPTZ,
  UNIQUE (kind, key)
);
-- Whether the place in a merchant's bank descriptions is the company's base (base) rather than where the purchase
-- happened. owner (from marking an away-from-home alert fine) always beats jev.
CREATE TABLE merchant_locations (
  merchant_key TEXT PRIMARY KEY,
  base BOOLEAN NOT NULL,
  source TEXT NOT NULL CHECK (source IN ('owner', 'jev'))
);
