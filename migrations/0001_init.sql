-- Consolidated schema. Add later changes as 0002_*.sql, 0003_*.sql, ...; each file runs in one transaction.
CREATE TABLE items (
  id TEXT PRIMARY KEY,
  last_synced_at TEXT, -- RFC 3339, last successful sync
  last_error TEXT NOT NULL DEFAULT '' -- last sync error or provider warnings, '' when healthy
);
CREATE TABLE accounts (
  id TEXT PRIMARY KEY,
  item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  institution TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  mask TEXT NOT NULL DEFAULT '',
  -- SimpleFIN never says what an account is, so money-tracker owns the type: sync writes guessed_type (guessAccountType),
  -- the user's dropdown choice lives in user_type and always wins; sync never touches user_type. Mirrors user_category/effective_category.
  guessed_type TEXT NOT NULL DEFAULT '', -- depository | credit | investment | loan | other
  guess_confident BOOLEAN NOT NULL DEFAULT false, -- false = the UI flags the account with "Check type"
  user_type TEXT NOT NULL DEFAULT '', -- manual choice from the same set; '' = use the guess
  -- Optional Jev fallback for non-confident guesses (jev.go): jev_asked_name is the name last sent ('' = never asked, so a rename re-asks),
  -- jev_type its accepted answer ('' = none); while jev_asked_name = name, sync and PATCH keep jev_type as the non-confident guess.
  jev_asked_name TEXT NOT NULL DEFAULT '',
  jev_type TEXT NOT NULL DEFAULT '',
  type TEXT GENERATED ALWAYS AS (CASE WHEN user_type != '' THEN user_type ELSE guessed_type END) STORED, -- effective type
  subtype TEXT NOT NULL DEFAULT '',
  current BIGINT, -- cents, signed: liabilities negative, so net worth = SUM(current)
  available BIGINT,
  currency TEXT NOT NULL DEFAULT 'USD'
);
CREATE TABLE balances (
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  date TEXT NOT NULL,
  current BIGINT NOT NULL, -- same sign convention as accounts.current
  PRIMARY KEY (account_id, date)
);
CREATE TABLE transactions (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  date TEXT NOT NULL, -- YYYY-MM-DD
  amount BIGINT NOT NULL, -- cents, positive = money in
  name TEXT NOT NULL,
  merchant TEXT NOT NULL DEFAULT '',
  pending BOOLEAN NOT NULL DEFAULT false,
  transfer_id TEXT, -- the matching transaction on another own account; NULL when not a transfer
  user_category TEXT NOT NULL DEFAULT '', -- manual choice; never overwritten by sync or rules
  rule_category TEXT NOT NULL DEFAULT '', -- winner among rules, then remembered merchant category; recomputed by applyRules
  merchant_key TEXT NOT NULL DEFAULT '', -- cleaned merchant key, set in Go by assignMerchantKeys; '' until then
  -- what the UI shows: manual > rule (incl. remembered merchant category) > '' (uncategorized)
  effective_category TEXT GENERATED ALWAYS AS (CASE WHEN user_category != '' THEN user_category ELSE rule_category END) STORED
);
CREATE INDEX transactions_date ON transactions(date);
CREATE INDEX transactions_merchant_key ON transactions(merchant_key);
-- SELECT * views freeze their column list at creation: after adding a transactions column in a later
-- migration, DROP VIEW cashflow and recreate it so the view exposes the new column.
CREATE VIEW cashflow AS SELECT * FROM transactions
  WHERE NOT pending AND transfer_id IS NULL;
CREATE TABLE rules (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  pattern TEXT NOT NULL, -- case-insensitive substring of merchant, else name
  category TEXT NOT NULL
);
CREATE UNIQUE INDEX rules_pattern ON rules(lower(pattern));
CREATE TABLE merchants (
  key TEXT PRIMARY KEY, -- lowercase cleaned name
  display_name TEXT NOT NULL, -- auto title-cased on first sight; PATCH renames it and sync never overwrites it
  merged_into TEXT NOT NULL DEFAULT '', -- key of the merchant this one was merged into ('' = itself is a target); kept flat, never chained
  category TEXT NOT NULL DEFAULT '' -- "remember" category from an accepted suggestion; folded into rule_category by applyRules
);
-- key -> the merchant it resolves to (itself unless merged), with that merchant's display name and category.
CREATE VIEW merchant_roots AS SELECT m.key, r.key AS root, r.display_name, r.category
  FROM merchants m JOIN merchants r ON r.key = COALESCE(NULLIF(m.merged_into, ''), m.key);
