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

## Internal loan API

Set `THOR_API_KEY` to enable `GET /api/v1/loans/{account}/contractual?as_of=YYYY-MM-DD`. An empty key leaves the API disabled. Callers send `Authorization: Bearer <THOR_API_KEY>`; they do not provide Fincloud credentials. TRS uses its configured Fincloud system account and the same `position.Service` as existing business flows.

`account` may be a primary or alternate Fincloud account. `as_of` controls contractual outstanding only. `repayment_history` contains full Fincloud repayment history, including payments after `as_of`. Financial JSON values are numbers with two decimal places.

```sh
curl -H 'Authorization: Bearer example-secret' \
  'http://localhost:8080/api/v1/loans/3080010000000123/contractual?as_of=2026-08-31'
```

Example response (fake data):

```json
{
  "requested_account": "3080010000000123",
  "primary_account": "3080010000000456",
  "as_of": "2026-08-31",
  "contract_rate": 12.50,
  "contractual_outstanding": 93456789.12,
  "position_source": "DWH",
  "repayment_history": [{"payment_date":"2026-09-01","principal":1000000.00,"interest":120000.00,"penalty":0.00,"early_termination_penalty":0.00,"dwp":0.00,"total_payment":1120000.00,"journal_number":"J-12345"}]
}
```

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
