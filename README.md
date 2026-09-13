# THOR Rate Sync

Goment-based internal banking application for contractual loan positions, bulk reports, current-day snapshots, and LPS archives.

## Architecture

- Local Goment users, Argon2id passwords, server-side sessions, RBAC, impersonation, and audit.
- Central Fincloud system session. Browser sessions never contain Fincloud credentials or session IDs.
- Read-only MSO opening/historical evidence.
- Read-only actual DWH H-1 evidence.
- Application-owned H snapshot refreshed from Fincloud `Loan Outstanding Details Report Today`.
- Pure exact-rational contractual calculator under `internal/contractual`.

## Setup

Requirements: Go 1.26.5+, Node.js 24+, npm, and MySQL 8+.

```sh
cp .env.example .env
npm install
make migrate
make admin
make dev
```

Set application DB, read-only DWH/MSO DSNs, and Fincloud system credentials in `.env`. Never commit secrets.

`MSO_INTEREST_TYPE_QUERY` and `MSO_DEBTOR_TYPE_QUERY` must be read-only `SELECT` statements with one `?` placeholder. Related operations fail clearly when either query is required but absent.

## Commands

```sh
make frontend-build
make build
make test
make test-integration
make lint
make verify
make migrate
make migrate-status
```

Production binary: `bin/trs`.

## Data rules

- Dates before `2025-10-12` use MSO.
- `2025-10-12` uses MSO EOD opening state.
- Later eligible flat loans reconstruct from MSO opening state, Fincloud schedule dates/repayments, and historical collectability.
- Historical exact positions use DWH. Today exact position/collectability uses local snapshot.
- Missing evidence returns an error. No current Fincloud balance fallback exists.

## Unresolved production validation

- Confirm actual DWH column names documented in `ETF_REWRITE_TECHNICAL_REQUIREMENTS.md`.
- Confirm MSO loan-master table/key used for `kre_sistem_bunga`.
- Supply `MSO_DEBTOR_TYPE_QUERY` from approved MSO schema.
- Validate contractual installment rounding against MSO. Calculator currently keeps exact rational values behind one rounding hook.
- Replace provisional numeric LPS debtor/DATI2 validation with authoritative exhaustive code sets.
- Confirm official LPS `ListHarian` request/response field names against reachable production service.
