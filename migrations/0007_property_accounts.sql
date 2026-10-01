-- Hand-sourced property accounts (a home): no connector; parcel is the county assessor's parcel number whose assessed value is the balance.
ALTER TABLE accounts ADD COLUMN parcel TEXT NOT NULL DEFAULT '';
