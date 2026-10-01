-- The owner's decisions on the Recurring page, keyed by merchant key (raw or merged-into root; a group matches
-- if any of its keys has one). cancelled: moved to Cancelled until it bills after marked_at. hidden: not recurring.
CREATE TABLE recurring_marks (
  merchant_key TEXT PRIMARY KEY,
  state TEXT NOT NULL CHECK (state IN ('cancelled', 'hidden')),
  marked_at TEXT NOT NULL -- YYYY-MM-DD, like transactions.date
);
