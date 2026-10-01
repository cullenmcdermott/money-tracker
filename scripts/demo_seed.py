#!/usr/bin/env python3
"""Print SQL for a year of made-up household finances, for README screenshots.

    python3 scripts/demo_seed.py | psql -q postgres://postgres@127.0.0.1:5433/money_readme

Run it against an empty, migrated database, then start the app on it: the server assigns merchant keys at startup.
Everything here is invented; the random seed is fixed so the screenshots are reproducible.
"""
import datetime as dt
import random

random.seed(7)
START, END = dt.date(2025, 9, 1), dt.date(2026, 9, 30)
ACCOUNTS = [  # id, institution, name, mask, type, opening balance (cents)
    ("chk", "Harbor Credit Union", "Everyday Checking", "4821", "depository", 640000),
    ("sav", "Harbor Credit Union", "High-Yield Savings", "9310", "depository", 1850000),
    ("card", "Northwind Bank", "Rewards Visa", "1177", "credit", -84000),
    ("ret", "Summit Investments", "401(k)", "5502", "investment", 8420000),
    ("brk", "Summit Investments", "Brokerage", "7745", "investment", 2310000),
    ("mtg", "Lakeview Home Loans", "Mortgage", "0638", "loan", -31250000),
    ("home", "County Assessor", "Home", "", "property", 48500000),
]
bal = {a[0]: a[5] for a in ACCOUNTS}
txs, balances, n = [], [], 0


def tx(day, acct, cents, name, category="", transfer=None):
    global n
    n += 1
    txs.append((f"demo:{n}", acct, day.isoformat(), cents, name, category, transfer))
    bal[acct] += cents


def transfer(day, src, dst, cents, out_name, in_name):
    tid = f"demo-t{n + 1}"
    tx(day, src, -cents, out_name, "Transfer", tid)
    tx(day, dst, cents, in_name, "Transfer", tid)


def amt(lo, hi):
    return -round(random.uniform(lo, hi) * 100)


def every(day, start, weeks):
    return day >= start and (day - start).days % (7 * weeks) == 0


day = START
while day <= END:
    dom, wd = day.day, day.weekday()
    # Income and fixed bills from checking.
    if every(day, dt.date(2025, 9, 5), 2):
        tx(day, "chk", 468000, "ACME PAYROLL", "Income")
        transfer(day, "chk", "ret", 65000, "401K CONTRIBUTION ACME", "401K CONTRIBUTION")
    if dom == 1:
        tx(day, "chk", -216000, "LAKEVIEW HOME LOANS MORTGAGE PMT", "Housing")
        tx(day, "mtg", 61000, "PRINCIPAL PAYMENT")
    if dom == 3:
        tx(day, "chk", -14200, "STATE MUTUAL AUTO INS", "Insurance")
    if dom == 8:
        winter = day.month in (11, 12, 1, 2)
        summer = day.month in (6, 7, 8)
        tx(day, "chk", amt(150, 210) if winter else amt(160, 240) if summer else amt(85, 120), "CITY POWER & LIGHT", "Utilities")
        tx(day, "chk", amt(48, 72), "METRO WATER UTILITY", "Utilities")
    if dom == 12:
        tx(day, "chk", -7000, "FIBERNET INTERNET", "Utilities")
    if dom == 15:
        transfer(day, "chk", "sav", 75000, "TRANSFER TO SAVINGS 9310", "TRANSFER FROM CHECKING 4821")
        transfer(day, "chk", "brk", 40000, "SUMMIT INVESTMENTS ACH", "FUNDS RECEIVED")
    if dom == 28:
        tx(day, "sav", round(bal["sav"] * 0.0035), "INTEREST PAYMENT", "Income")
    if dom == 20:  # pay the card in full
        transfer(day, "chk", "card", -bal["card"], "NORTHWIND BANK CARD PAYMENT", "PAYMENT THANK YOU")
    # Everyday spending on the card.
    if wd in (2, 6):
        tx(day, "card", amt(55, 165), random.choice(["TRADER JOE'S #512", "SAFEWAY #1834", "SAFEWAY #1834"]), "Groceries")
    if wd in (4, 5) and random.random() < 0.8:
        tx(day, "card", amt(24, 95), random.choice(["CHIPOTLE 2231", "PIZZERIA LUCCA", "SUSHI GEN", "TAQUERIA EL SOL", "THE PUBLIC HOUSE"]), "Dining")
    if wd < 5 and random.random() < 0.35:
        tx(day, "card", amt(4.5, 9), "BLUE BOTTLE COFFEE", "Coffee Shops")
    if every(day, dt.date(2025, 9, 3), 2):
        tx(day, "card", amt(38, 62), "SHELL OIL 57442", "Transportation")
    if random.random() < 0.18:
        tx(day, "card", amt(12, 140), random.choice(["AMAZON MKTPL", "AMAZON MKTPL", "TARGET 00012", "REI #87"]), "Shopping")
    if dom == 9:
        tx(day, "card", -1549, "NETFLIX.COM", "Subscriptions")
        tx(day, "card", -1199, "SPOTIFY USA", "Subscriptions")
    if dom == 21:
        tx(day, "card", -299, "APPLE.COM/BILL ICLOUD", "Subscriptions")
        tx(day, "card", amt(48, 56), "CHEWY.COM", "Pets")
    if every(day, dt.date(2025, 9, 13), 6):
        tx(day, "card", -3200, "GREAT CLIPS #4410", "Haircuts")
    if random.random() < 0.04:
        tx(day, "card", amt(14, 60), "CVS PHARMACY #2207", "Health")
    if random.random() < 0.03:
        tx(day, "card", amt(28, 70), "AMC THEATRES", "Entertainment")
    if day == dt.date(2026, 6, 14):
        tx(day, "card", -61240, "DELTA AIR LINES", "Travel")
    if dt.date(2026, 7, 2) <= day <= dt.date(2026, 7, 6):
        tx(day, "card", amt(180, 230), "HARBORVIEW HOTEL", "Travel")
    # Investments: quarterly dividends, then a noisy market drift.
    if dom == 25 and day.month % 3 == 0:
        tx(day, "brk", round(bal["brk"] * 0.0042), "DIVIDEND VTI")
    if day.month == 1 and dom == 1:  # the assessor's yearly revaluation
        bal["home"] = int(round(bal["home"] * 1.046, -5))
    for a in ("ret", "brk"):
        bal[a] = round(bal[a] * (1 + random.gauss(0.0006, 0.007)))
    balances += [(a[0], day.isoformat(), bal[a[0]]) for a in ACCOUNTS]
    day += dt.timedelta(days=1)

# The last five days are left for Review to suggest categories.
review = (END - dt.timedelta(days=5)).isoformat()
txs = [t if t[2] < review or t[5] in ("Income", "Transfer", "Housing") else t[:5] + ("", t[6]) for t in txs]


def q(v):
    return "NULL" if v is None else str(v) if isinstance(v, int) else "'" + str(v).replace("'", "''") + "'"


print("BEGIN;")
print(f"INSERT INTO items(id,last_synced_at) VALUES ('simplefin','{END.isoformat()}T14:00:00Z');")
for a in ACCOUNTS:
    print(f"INSERT INTO accounts(id,item_id,institution,name,mask,guessed_type,guess_confident,user_type,current,available)"
          f" VALUES ({q(a[0])},'simplefin',{q(a[1])},{q(a[2])},{q(a[3])},{q(a[4])},true,{q(a[4])},{bal[a[0]]},{bal[a[0]]});")
for t in txs:
    print(f"INSERT INTO transactions(id,account_id,date,amount,name,user_category,transfer_id) VALUES ({','.join(q(v) for v in t)});")
for b in balances:
    print(f"INSERT INTO balances(account_id,date,current) VALUES ({','.join(q(v) for v in b)});")
print("COMMIT;")
