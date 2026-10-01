# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

One person (the owner) tracking their own household finances across several banks, card issuers and brokerages. Two usage moments, both matter:
- **Glance** (often on a phone): "am I okay this month?"
- **Review** (every week or two, on a desktop): look at the month's income and spending, see how net worth moved, fix categories.

## Product Purpose

A self-hosted replacement for Monarch Money. It shows income vs. expenses per month and net worth across all accounts. Success means the high-level picture is clear at a glance, and the details are there only when the owner goes looking for them.

## Positioning

A private tool for one person, running on the owner's own hardware. It has no ads, upsells or social features, and its design never pushes the owner to act. It can be quieter and more opinionated than a commercial app because it has exactly one user.

## Operating Context

- The owner is the only user, behind Authelia on a homelab, so there's no auth UI in the app.
- Data comes from SimpleFIN Bridge (no categories). Syncs run daily, plus a manual sync.
- History starts at the first sync. SimpleFIN backfills about 89 days of transactions, but only has balances from the first sync onward, so net worth history is short at first.

## Capabilities and Constraints

- Transfers between the owner's own accounts (matched pairs, including credit card payments) don't count as income or expense. Pending transactions are excluded from cash flow.
- Amounts are stored as cents. Liabilities have negative balances, so net worth is the sum of all balances.
- Planned: categorization (rules, manual recategorizing, an inbox of uncategorized transactions), manual accounts (mortgage and home value), CSV import. Later: holdings, budgets, alerts.
- Stack: Vite + vanilla JS + Chart.js, embedded in the Go binary. No framework.

## Evidence on Hand

Only the SimpleFIN demo data exists so far; the owner's real data arrives with the first real sync. The app must never show sample numbers presented as real data.

## Product Principles

1. **Overview first, details on request.** The landing page answers two questions: this month's income vs. spending, and the net worth trend. Everything else lives in its own view, one click away.
2. **Calm, not dense.** Put few numbers on the page, make each one legible, and make charts more important than tables.
3. **Honest about gaps.** Show sync age, errors, pending transactions and short history plainly. Never smooth them over.
4. **Plumbing stays out of the way.** Connections and sync controls belong in Settings, not on the overview.
