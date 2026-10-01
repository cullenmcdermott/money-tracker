-- One row per GET /accounts sent to SimpleFIN Bridge, which expects 24 or fewer per day; rows older than a day are pruned.
CREATE TABLE simplefin_requests (at TIMESTAMPTZ NOT NULL DEFAULT now());
