# Observability

The application writes operational logs as structured records to stdout. It does
not ship logs, depend on a log backend, or expose metrics or traces.

```
app (slog) -> stdout -> journald / container runtime -> collector -> backend
```

## Two kinds of records

| Concern | Answers | Authoritative store |
|---|---|---|
| Audit log | Who logged in, changed access, or ran an inquiry, SLIK, or LPS | `audit_logs` table |
| SLIK job state | Progress and failure reason per job | `slik_jobs` table |
| Operational logs | What started, finished, how long it took, and the outcome | stdout (this document) |

## Format

- `APP_ENV=development`: human-readable text at DEBUG.
- Any other environment: JSON at INFO.

Every record carries `time`, `level`, `msg`, `service` (from `APP_NAME`), and
`environment` (from `APP_ENV`). Lifecycle records add `event`, a stable
dot-separated identity that never contains IDs or values. Filter on `event`, not
`msg`.

## Events

| Event | Level | Purpose |
|---|---|---|
| `app.starting` / `app.started` | INFO | Process start; `app.started` includes `address` |
| `app.database.connected` | INFO | DB destination (`network`, `host`, `port`, `socket`, `database`; never credentials) |
| `app.stopping` / `app.http.drained` | INFO | Graceful shutdown start / HTTP drain end (`duration_ms`) |
| `http.request.completed` | INFO | One per request: `request_id`, `method`, `route`, `status`, `duration_ms`, `response_bytes`, `protocol` |
| `http.request.error` | ERROR | Handler error: `request_id`, `method`, `route`, `operation`, safe error fields |
| `http.request.panic` | ERROR | Recovered panic: `panic_type` and `stack`; never the panic value |

Successful access records are suppressed for `/health`, `/ready`, `/static/*`,
and the SLIK status poll `/slik/{id}/status` (`quietRoutes` in
`internal/server/middleware.go`; add new HTMX polling routes there). Responses
with status >= 400 on those routes are still logged.

`loan position source selected` records the routing decision for each loan
position (source, reason, schedule row counts) and is the first place to look
when an inquiry shows unexpected data.

## Sensitive data

Operational logs never contain passwords, Fincloud session IDs, cookies, raw
request or response bodies, or query strings. HTTP records use the chi route
pattern (`/slik/{id}`, or `unmatched`), never the raw path.

Error text can quote SQL, account numbers, or upstream payloads, so no
operational record contains an error message. Every error logged goes through
`logging.Err`, which emits only:

- `error_type`: the Go type of the first non-`fmt` wrapper in the chain;
- `error_kind`: `cancelled` or `deadline_exceeded` for context errors;
- for MySQL errors, `mysql_error` (number) and `sqlstate`, never the server message.

Rich failure detail belongs in the durable stores above (for example
`slik_jobs.failure_reason`).

## Not implemented

- Metrics and tracing; correlate with `request_id`.
- Log shipping; configure a collector at deployment.
