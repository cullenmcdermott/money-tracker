-- The Plan page. plan is one row: the owner's birth year, the numbers they set (any left out come from their
-- data or a default) and their events, as one JSON document. goals are savings targets, each linked to accounts
-- by percentage; an account's percentages across goals total at most 100 (checked by the API).
CREATE TABLE plan (
  id INT PRIMARY KEY CHECK (id = 1),
  doc JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE goals (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name TEXT NOT NULL,
  target_cents BIGINT CHECK (target_cents > 0),
  target_months INT CHECK (target_months > 0), -- months of expenses, following the plan's spending
  target_date TEXT NOT NULL DEFAULT '', -- YYYY-MM, '' = none
  CHECK ((target_cents IS NULL) <> (target_months IS NULL))
);
CREATE TABLE goal_accounts (
  goal_id BIGINT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  pct INT NOT NULL CHECK (pct BETWEEN 1 AND 100),
  PRIMARY KEY (goal_id, account_id)
);
