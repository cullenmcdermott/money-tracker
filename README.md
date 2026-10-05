# Money Tracker

A self-hosted personal finance dashboard: one household's checking, credit card, investment and loan accounts in one place. Accounts sync through [SimpleFIN Bridge](https://beta-bridge.simplefin.org), everything is stored in your own Postgres, and you sign in with your own OIDC provider. Built to replace Monarch Money for a single household.

![Overview: a month of income flowing to spending categories and savings](docs/screenshots/overview.jpg)

- **Overview** shows where a month's or year's income went, plus net worth and cash flow for the 12 months ending there.
- **Spending** compares each category with its six-month average, and **Recurring** finds subscriptions, bills and paychecks from the history: what's coming up this week, what's new or went up, and bills that vary month to month. Mark a charge cancelled (it comes back flagged if it bills again) or not recurring.
- **Investments** splits each account's growth into what you added, dividends and market change.
- **Review** suggests categories from your own history and keyword rules. An optional AI fallback ([TypeSafe Jev](https://docs.typesafe.ai)) is off unless you add a key.
- Transfers between your own accounts are paired and kept out of income and spending. Nightly backups can be restored from the UI.

| | |
| --- | --- |
| ![Spending by category](docs/screenshots/spending.jpg) | ![Recurring charges](docs/screenshots/recurring.jpg) |
| ![Investments](docs/screenshots/investments.jpg) | ![Review suggestions](docs/screenshots/review.jpg) |

The screenshots use invented data from `scripts/demo_seed.py`.

## Quick start

### Try it locally

You need Go, Node, Postgres 17 and [just](https://just.systems). `flox activate` provides all four.

```sh
just demo
```

This starts a local Postgres in `data/pg`, loads SimpleFIN's public demo accounts into a separate `money_demo` database and opens the app at http://localhost:5188 with sign-in turned off. No secrets are needed.

### Run it for real

The image is `ghcr.io/cullenmcdermott/money-tracker` (amd64 and arm64, runs as uid 65532). It needs Postgres, a SimpleFIN access URL and an OIDC client:

```sh
docker run -p 8080:8080 -v money-data:/data \
  -e DATABASE_URL='postgres://money:...@db:5432/money' \
  -e SIMPLEFIN_ACCESS_URL='https://user:pass@beta-bridge.simplefin.org/simplefin' \
  -e OIDC_ISSUER=https://id.example.com -e OIDC_CLIENT_ID=money-tracker -e OIDC_CLIENT_SECRET=... \
  -e OIDC_REDIRECT_URL=https://money.example.com/auth/callback -e OIDC_ALLOWED_GROUPS=money \
  -e SESSION_SECRET=... \
  ghcr.io/cullenmcdermott/money-tracker:0.1.2
```

1. Get the access URL by [claiming a SimpleFIN setup token](#simplefin).
2. Create the OIDC client as described in [Sign-in](#sign-in), and generate `SESSION_SECRET` once with `openssl rand -base64 32`.
3. Open the app. The first sync runs within the hour, or click **Sync now** in Settings. SimpleFIN returns about 90 days of history; to bring older history from Monarch, see [Importing from Monarch](#importing-from-monarch).

Put a TLS-terminating proxy in front of it. The Kubernetes manifests this app runs on (CNPG Postgres, External Secrets, Traefik) live in the homelab repo.

### Develop

| Command | What it does |
| --- | --- |
| `just bootstrap` | First-time setup. It claims your SimpleFIN token, generates `SESSION_SECRET` and collects the OIDC details and an optional Jev key. It stores them as fields of one 1Password item and writes `.env.op` / `.env.op.auth`, which hold only `op://` references. It never prints a secret. |
| `just dev-op` | Hot-reload dev against your real data, with secrets injected by `op run` and sign-in off. http://localhost:5188 |
| `just local` | The production build with real sign-in at http://127.0.0.1:8188. Add `http://127.0.0.1:8188/auth/callback` to the OIDC client first. |
| `just dev` | Plain dev. It reads `SIMPLEFIN_ACCESS_URL` from `.env` if set. |
| `just test` | `go vet` and the tests, against a throwaway Postgres on port 5434. |

Sign-in can only be turned off (`AUTH_DISABLED=true`) when the server listens on a loopback address and no OIDC issuer is set. Even then it answers only requests addressed to `localhost`.

## SimpleFIN

The app reads one long-lived access URL (`https://user:pass@host/path`) from `SIMPLEFIN_ACCESS_URL`. It is checked at startup, never logged, and never stored in the database, and there is nowhere in the UI to paste it.

1. Create a setup token in your [SimpleFIN Bridge](https://beta-bridge.simplefin.org) account.
2. Redeem it once and keep the output, because a token can only be claimed once:
   ```sh
   pbpaste | just simplefin-claim
   # or with the image:
   pbpaste | docker run --rm -i ghcr.io/cullenmcdermott/money-tracker simplefin-claim
   ```
   The token is read from stdin, which keeps it out of shell history and `ps`, and only the access URL is printed.
3. Give it to the app as `SIMPLEFIN_ACCESS_URL`.

The server checks once an hour, at a random minute. It syncs when the last successful sync is more than 23 hours old and no request has gone to SimpleFIN in the past 4 hours. **Sync now** in Settings syncs immediately. Each sync re-reads the last 14 days, so late-posting transactions are picked up.

## Sign-in

The app signs in with OIDC. It was built for [Pocket ID](https://pocket-id.org), but any provider with discovery and PKCE works. It uses the authorization code flow with PKCE, state and nonce. The session is an encrypted cookie valid for 30 days, and nothing is stored server-side. Every route except `/api/health` and `/auth/*` requires a session, including the page itself.

1. In your provider, create an OIDC client with callback URL `<base>/auth/callback`. Optionally add `<base>/auth/signed-out` as the logout callback.
2. Create a group (e.g. `money`), add the people who should get in, and restrict the client to it.
3. Set `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` (leave it out for a public client), `OIDC_REDIRECT_URL`, `SESSION_SECRET` and `OIDC_ALLOWED_GROUPS=money`.

`OIDC_ALLOWED_EMAILS` also works. Groups are safer, because in some providers users can change their own email. Anyone not allowed gets a 403 page. Rotating `SESSION_SECRET` signs everyone out.

## Alerts

After each sync the app looks for charges worth a second look, using only your own history. Nothing is sent outside the app; open alerts show as one line on the Overview and in full under Transactions → Alerts.

- **New merchant:** the first charge from a merchant, $250 or more.
- **Unusual amount:** at least 3× the merchant's largest earlier charge and $50 more.
- **Possible test charge:** a charge under $5 at a new merchant, followed within 3 days by a new-merchant charge of $250 or more on the same account.
- **Possible duplicate:** same merchant, amount and account on the same day, $20 or more.
- **Away from home:** a charge whose description ends in a state other than your usual one, with no other charges there within 3 days. It waits 2 days for others to post first.
- **Spending running high:** from the 7th of the month, a category's month to date is at least 1.25× the same days of the last 3 months, and $100 more.

Only charges from the last 14 days are checked, and new-merchant, amount and test-charge alerts wait until an account has 60 days of history. **Looks fine** removes one for good (for an away alert, it also remembers that merchant as based elsewhere); **Undo** brings it back. With Jev on, each charge alert also gets a verdict (normal, unusual or suspicious), and confident "normal" ones are listed apart.

## Smart suggestions (Jev, optional)

Set `JEV_API_KEY` to let Jev answer what your own history and keyword rules can't. It is off by default. Answers are saved per merchant, and those below `JEV_MIN_CONFIDENCE` are shown as low-confidence guesses. Settings shows the token usage and estimated cost.

Requests are batched, up to 20 questions each. Exactly what is sent:

- For merchants with no local suggestion: the display name, up to 3 raw bank descriptions, the typical amount and whether it's income or spending.
- How often each merchant charges, and `JEV_HOME_LOCATION` if set.
- Up to 3 merchants you already categorized per category, as examples.
- For accounts whose type is uncertain: the institution, account name, and whether the balance is positive, negative or zero.
- For merchants in an alert whose descriptions name a place: the display name and up to 3 bank descriptions, to ask whether that place is just the company's base.
- For each new charge alert: its bank description, amount, reasons, whether it was on a credit card or bank account, your top 5 spending categories of the last 90 days, your usual state, and `JEV_HOME_LOCATION` if set.

Never sent: balances, amounts for accounts, transaction ids or dates. Errors and timeouts are logged without the key and never break suggestions or sync.

## Plan

The Plan page projects the money in your cash and investment accounts year by year, in today's dollars, to see whether it lasts and what retirement age works. It asks only for your birth year. Everything else starts from your data:

- **Income and spending:** the monthly average of your last 12 complete months of cash flow (fewer if that's all there is, and the page says how many). Interest paid into a deposit account is left out of income: it sets that account's growth instead, so it isn't counted twice. If it works out to more than 6% a year of the account's average balance, it isn't that account's own interest (a dividend from an unconnected brokerage, say) and stays in income.
- **Balances:** each account's current balance. Checking, savings and investment accounts are in; cards, loans and property are out unless you include them. This year's months behind you are already in the balances, so only the rest of the year is projected.
- **Added a year:** each investment account's money from outside your own accounts over those months: payroll deposits, and ESPP share sales. Left out: RSU share sales (future vests come from the unvested stock below), moves between your own accounts (journals, Roth conversions, withdrawals and transfers, already part of the surplus or not new money), and share sales that follow neither an RSU vest nor an ESPP purchase in the same account, which the page lists for you to place. Vests and ESPP purchases are told apart by the $0 deposit each sale follows, not by the account's name: in an account that has had both, a sale belongs to a deposit in the 7 days up to it, and is left for you to place when there is none or both.
- **Kind and growth:** guessed from the account's name (Roth; 401(k), 403(b), IRA, HSA and the like as pre-tax; other investments as taxable; deposit accounts as cash), growing 7% a year for investments. A cash account grows at the rate of the interest it paid on its average balance over those months, or 2% for a savings account with no interest in the data, 0% otherwise.
- **Unvested stock:** the Investments page's unvested total at 65% after tax, vesting at the pace of your RSU sales over the same months (over 4 years when there are none), until you retire; what hasn't vested by then is forfeited. No new grants are assumed.

The page's "Where the numbers come from" section shows all of this: the largest sources behind income (to spot money from your own accounts, which the Transfer category takes out), what each account's contributions are made of, and what was left out and why.

Where there is no data, it uses defaults close to Monarch's: retirement at 65, spending in retirement at 100% of today's plus $6,500 a year, Social Security of $2,000 a month from 67, 3% inflation, a plan to age 90. Shortfalls are withdrawn from extra savings, then cash, taxable, pre-tax and Roth accounts, with flat taxes (12%, 20%, 0%) and a 10% penalty on pre-tax and Roth withdrawals before 59½ (65 for an HSA).

Every number can be changed. A change is a what-if: the chart shows it next to a dashed line for your data alone, and nothing is saved until you keep it. You can add one-time expenses or income and yearly changes (a mortgage paid off, college years). Goals track money set aside for something against the balances of the accounts you link to them, with the saving needed each month and your pace over the last 6 months.

It is a direction, not a prediction: no market swings, no tax brackets, Roth conversions or required withdrawals, and one person.

## Holdings and allocation

For brokerages that send holdings through SimpleFIN, the Investments page lists each account's latest positions with their value and, where the brokerage sends a cost basis, the unrealized gain. A cost of zero or none counts as unknown (money market funds often report zero), and those positions are left out of the gain with their value noted. Unvested stock awards (zero shares with a value, described as restricted stock or RSUs) are listed apart as an estimate and left out of net worth, gains and allocation.

Allocation splits holdings into US stock, international stock, bonds, cash and other. A split comes from, in order: one you set on the Investments page; a money market fund (cash); or a lookup of the fund's latest SEC N-PORT filing, refreshed every 120 days. A company's own stock counts as 100% US stock until you override it. Funds that don't file with the SEC, like most 401(k) collective trusts, stay unclassified until you set a split. A split you set doesn't follow a target-date fund's glide path.

SEC lookups are off unless `SEC_USER_AGENT` is set to your name and email, which the SEC requires of every client. Then, after each sync, the app downloads the SEC's public ticker lists (once a day) and the filings of held funds with no recent split. Only fund tickers and series ids are sent, at most 5 requests a second. Lookups tell the SEC which funds someone holds, which is why they are opt-in.

## Backups

Every night at `BACKUP_TIME`, and on demand from Settings, the server writes `BACKUP_DIR/money-YYYYMMDDTHHMMSSZ.tar.gz`. It contains a consistent CSV dump of every table and a manifest with the schema version. Only the newest `BACKUP_KEEP` are kept, and Settings warns if the last good backup is more than 36 hours old.

**Restore** replaces all data. Use Settings → Backups → **Restore** (or **Upload and restore**), or the CLI:

```sh
money-tracker restore data/backups/money-20260929T030000Z.tar.gz --yes
```

Before touching anything, a restore checks that the archive is valid and from the same schema version. It then takes a `-pre-restore` safety backup and reloads everything in one transaction, so a failure changes nothing. Backups hold all your financial data, so protect `BACKUP_DIR`.

## Importing from Monarch

Export transactions and balances from Monarch, then:

```sh
money-tracker import-monarch --transactions Transactions.csv --balances Balances.csv [--dry-run]
```

- Monarch accounts are matched to SimpleFIN accounts by their last digits, or else by name.
- Transactions are only imported from before each account's first SimpleFIN transaction.
- Monarch categories are mapped onto this app's.
- Re-running updates rows instead of duplicating them, and never overwrites a category you chose since.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `DATABASE_URL` | required | Postgres connection string. Migrations run at startup. |
| `ADDR` | `:8080` | Listen address. |
| `SIMPLEFIN_ACCESS_URL` | unset | Without it, nothing syncs. |
| `OIDC_*`, `SESSION_SECRET` | required | See [Sign-in](#sign-in). |
| `BACKUP_DIR` | `data/backups` (`/data/backups` in the image) | Must be writable by uid 65532 in the container. |
| `BACKUP_KEEP` | `14` | |
| `BACKUP_TIME` | `03:00` | `HH:MM`, server local time. The image has no time zone data, so this is UTC there. |
| `BACKUP_MAX_UPLOAD` | `1073741824` | Largest restore upload, in bytes. |
| `ASSESSOR_URL` | unset | A county assessor's public ArcGIS parcel layer query endpoint (`…/FeatureServer/<layer>/query`). Turns on daily home value refresh for property accounts imported with `import-monarch --property`. |
| `ASSESSOR_FIELDS` | `PARCEL,TOTALVALUE,PROPYEAR` | That layer's parcel number, total assessed value and tax year fields, comma-separated. |
| `JEV_API_KEY` | unset | Turns Jev on. |
| `JEV_HOME_LOCATION` | unset | For example `Denver, Colorado`. Lets Jev tell trips from local spending. |
| `JEV_MIN_CONFIDENCE` | `0.5` | |
| `JEV_MODEL`, `JEV_URL` | `jev-latest`, TypeSafe's endpoint | |
| `JEV_PRICE_INPUT_PER_MTOK`, `JEV_PRICE_OUTPUT_PER_MTOK` | `0.042`, `0` | Used for the cost estimate in Settings. |
| `SEC_USER_AGENT` | unset | Your name and email, e.g. `Jane Doe jane@example.com`. Turns on SEC fund lookups (see [Holdings and allocation](#holdings-and-allocation)). |

## Releases

CI runs on [Depot CI](https://depot.dev/docs/ci/overview) (`.depot/workflows/ci.yml`). Every PR and push runs vet and the tests. Pushes to `main` and `v*` tags also build the multi-arch image with provenance and SBOM, push it to GHCR and sign the digest with the repo's cosign key:

```sh
cosign verify --key cosign.pub ghcr.io/cullenmcdermott/money-tracker@sha256:<digest>
```

Actions are pinned to commit SHAs and base images to digests, so bump them by hand. Deploy by digest.

CI secrets live in Depot, not GitHub: `GHCR_TOKEN`, `COSIGN_PRIVATE_KEY` and `COSIGN_PASSWORD`. To rotate the key, run `cosign generate-key-pair`, store both values with `depot ci secrets add --repo cullenmcdermott/money-tracker`, and commit the new `cosign.pub`.
