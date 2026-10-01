-- Jev's answer per merchant (target key), reused until the user asks Jev again. category '' = no confident answer.
CREATE TABLE jev_suggestions (
  merchant_key TEXT PRIMARY KEY,
  category TEXT NOT NULL,
  confidence DOUBLE PRECISION NOT NULL DEFAULT 0,
  probabilities JSONB,
  asked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
