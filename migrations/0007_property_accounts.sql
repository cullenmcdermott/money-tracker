-- Hand-sourced property accounts (a home): no connector; parcel is the Ada County parcel whose assessed value is the balance.
ALTER TABLE accounts ADD COLUMN parcel TEXT NOT NULL DEFAULT '';
