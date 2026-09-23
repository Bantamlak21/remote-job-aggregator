# remote-job-aggregator

Autonomous remote-job aggregation platform, built as a Go modular monolith.
Discovers, normalizes, filters, ranks, and stores remote job postings from
ATS providers, prioritizing geographic-eligibility correctness (with a
default focus on Ethiopia) and data quality over raw volume.

**Current status: Phase 2 — company persistence, HTTP infrastructure,
discovery.** Phase 1 (config, logging, database pool, migrations, CLI
foundation) is done. Phase 2 adds a repository for companies and their
ATS boards, a shared pooled/retrying HTTP client, and a seed-file-driven
discovery mechanism that validates candidate boards and persists the
ones that check out. There is no ATS integration, ingestion, filtering,
ranking, scheduling, or notification code yet — those land in later
phases.

## Requirements

- Go 1.27+
- Docker + Docker Compose (for PostgreSQL)

## Running locally

```bash
cp .env.example .env   # edit if needed
docker compose up -d db
export $(grep -v '^#' .env | xargs)   # or use a tool like direnv
make migrate-up
make run
```

`aggregator run` connects to the database and blocks until it receives
SIGINT/SIGTERM, at which point it closes the pool and exits. There is no
ingestion loop yet, so this currently does nothing beyond holding the
connection open.

## Commands

```
aggregator run                        # connect, then block until shutdown signal
aggregator migrate-up                 # apply pending migrations
aggregator migrate-down --yes         # roll back one migration (requires confirmation)
aggregator migrate-down --yes --all   # roll back EVERY migration, dropping all tables
aggregator migrate-force <version>    # clear a "dirty" schema_migrations state
                                       # after an interrupted migrate-up/-down, without
                                       # running any migration
aggregator discover [seed-file]       # validate candidate ATS boards from a seed file
                                       # (default configs/seed_companies.json) and persist
                                       # the ones that respond
aggregator search-discover [names-file]  # find each company's ATS board via the Serper
                                       # search API (default configs/company_names.txt),
                                       # then the same validate-and-persist as discover
aggregator serve                      # start the public, read-only job API (see docs/api.md)
```

`migrate-down` always requires `--yes`: rolling back even one migration is
a `DROP TABLE`-class operation (this repo currently has exactly one
migration, so a single step already drops every table — `--all` only
matters once there's more than one).

All commands respond to a first Ctrl-C/SIGTERM by cancelling their
context; a second one forces an immediate exit regardless of what the
process is doing. `migrate-force` is a single near-instant metadata
update, so that context has nothing to do for it. `migrate-up` and
`migrate-down` use it to stop gracefully between migration files instead
of dying mid-migration — and, critically, they treat a canceled context
as a failure even if golang-migrate itself reports success: a graceful
stop makes the underlying library call return the same nil it would
return on genuine completion, so without this check a signal landing
mid-migration would log "migrations applied" (or "rolled back") and
exit 0 having done partial or no work — found live, on the one command
gated behind `--yes` specifically because it's destructive. That first
signal also isn't guaranteed to land promptly: golang-migrate's own lock
acquisition ignores the context it's given and runs with
`context.Background()` regardless, so a migration blocked waiting on
another process's lock is not interruptible by a first signal at all.
Verified live: holding an `ACCESS EXCLUSIVE` lock on `schema_migrations`
and sending SIGTERM to a blocked `migrate-down` left it running; a second
SIGTERM forced it to exit before it had touched the table. Either way —
blocked on a lock, or interrupted mid-run — the command exits non-zero
and is safe to simply re-run; `migrate-up`/`migrate-down` are idempotent.
If a migration is ever left dirty anyway (a hard kill, `kill -9`, a crash
mid-statement), `migrate-force` recovers it without a manual `psql`
session.

## Environment variables

See [.env.example](.env.example) for the full list with defaults. All
validation happens in `internal/config`; an invalid or missing required
value fails fast with a specific error message (multiple problems are
reported together, not one at a time), and that message never includes a
raw credential even when the value itself was the problem.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `APP_ENV` | no | `development` | one of `development`, `staging`, `production` |
| `DATABASE_URL` | **yes** | — | `postgres://` or `postgresql://` |
| `DB_MAX_OPEN_CONNS` | no | `10` | 1 – 2147483647 |
| `DB_MIN_CONNS` | no | `2` | a floor pgxpool tries to keep warm, not a cap — pgxpool has no idle-connection ceiling; must not exceed `DB_MAX_OPEN_CONNS` |
| `DB_CONN_MAX_LIFETIME` | no | `30m` | must be positive |
| `DB_CONN_MAX_IDLE_TIME` | no | `5m` | must be positive |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | no | `json` | `json`, `text` |
| `SHUTDOWN_TIMEOUT` | no | `15s` | max wait during graceful shutdown |
| `HTTP_TIMEOUT` | no | `10s` | whole-round-trip budget for the shared outbound HTTP client; must be positive |
| `HTTP_MAX_RESPONSE_SIZE` | no | `5242880` (5 MiB) | bytes; independent of `HTTP_TIMEOUT` — this bounds size, not time; must be positive |
| `HTTP_USER_AGENT` | no | `remote-job-aggregator/1.0 (+https://github.com/Bantamlak12/remote-job-aggregator)` | sent on every outbound request; must not be blank |
| `DISCOVERY_WORKERS` | no | `5` | bounded concurrency for discovery's HTTP probes; 1 – 100 |
| `SERPER_API_KEY` | no | — | only for `search-discover`; from https://serper.dev/api-keys |
| `API_ADDR` | no | `:8080` | only for `serve`; must be a valid `host:port` |
| `CORS_ALLOWED_ORIGIN` | no | `http://localhost:5173` | only for `serve`; a single explicit origin, never `*` |
| `TEST_DATABASE_URL` | no | — | integration tests only; database name must end in `_test` |

Config values passed via `DATABASE_URL`'s own query string (e.g.
`pool_max_conns`) are intentionally overridden by the `DB_*` variables
above — `internal/config` is the single source of truth for pool sizing,
not the DSN.

## Database

PostgreSQL is the source of truth. Schema is managed with versioned SQL
migrations under [migrations/](migrations/), embedded into the binary and
applied via `golang-migrate` (never automatically at process startup).

Phase 1 schema:

- **companies** — organizations known to the system. Unique on
  `lower(btrim(name))` and, when present, on a normalized form of
  `website` (case-folded, trailing slash stripped — it does not
  normalize `http` vs `https` or `www` vs bare host, since that needs
  real URL parsing).
- **target_companies** — a company's ATS boards. A company may have
  several. Unique on `(ats_provider, lower(btrim(external_board_id)))`.
- **jobs** — normalized postings. Identity is `(source, source_job_id)`
  per the provider's own stable ID, with a `canonical_url` fallback —
  never company name + title (see CLAUDE.md §9). Composite foreign keys
  tie `(target_company_id, company_id)` and `(target_company_id, source)`
  back to `target_companies`, so a job can't be attributed to a company
  that doesn't own its board, and its `source` can't drift from the
  board's own `ats_provider` — both would otherwise silently corrupt the
  identity constraint above. `remote_type`, `employment_type`, and
  `status` are DB-level `CHECK` constrained, as are non-blank identity
  fields, `status`/`closed_at` consistency, and `last_seen_at >=
  first_seen_at`. `updated_at` on every table is maintained by a trigger
  (only on real changes, not a no-op `UPDATE`), not application code.

There is deliberately no `job_eligibility` table yet — nothing populates
it until the geographic-classification phase lands.

## Testing

```bash
make test         # unit tests only — no database required
docker compose up -d db
make test-integration       # or: TEST_DATABASE_URL=...aggregator_test go test -p 1 ./...
make test-integration-race
```

Database and migration tests run against a real PostgreSQL instance
(skipped automatically if `TEST_DATABASE_URL` is unset) — they are not
mocked, per project testing policy. **`-p 1` is required whenever
`TEST_DATABASE_URL` is set and more than one package's tests run
together**: `internal/database`, `internal/company`, and any future
package with its own DB-integration tests all reset the schema
(`MigrateDown`/`MigrateUp`) against the *same* `aggregator_test`
database, and `go test`'s default package-level parallelism runs each
package's tests as a separate concurrent process with no interlock
between them — without `-p 1` they will drop each other's tables
mid-run. `make test-integration`/`test-integration-race` already pass
it; a bare `go test ./...` with `TEST_DATABASE_URL` set does not and
will intermittently fail with errors like `relation "companies" does
not exist` — that failure means this, not a real bug, if it ever shows
up. These tests call `migrate-down` and drop tables, so
`TEST_DATABASE_URL`'s database name **must** end in `_test`; the test
suite refuses to run otherwise. `docker compose up -d
db` provisions `aggregator_test` alongside the main `aggregator` database
automatically (see `scripts/postgres-initdb/`) — never point
`TEST_DATABASE_URL` at the same database `DATABASE_URL` uses.

## Discovery

```bash
aggregator discover                          # uses configs/seed_companies.json
aggregator discover path/to/custom_seed.json
```

The seed file is a JSON array of candidates:

```json
[
  {
    "company_name": "Spotify",
    "website": "https://www.spotify.com",
    "ats_provider": "lever",
    "external_board_id": "spotify",
    "board_url": "https://jobs.lever.co/spotify"
  }
]
```

`website` is optional; the other four fields are required, and
`board_url` must be an `http(s)` URL. Every candidate is validated
before any of them are probed — one bad entry fails the whole load, so a
typo is caught before wasting HTTP round trips on the rest of the file.
For each valid candidate, `discover` probes `board_url` (`HEAD`, falling
back to `GET` — some ATS-hosted boards don't handle `HEAD` reliably per
the ATS ecosystem generally; the fallback mechanism itself is tested
against a controlled fake server, but live spot-checks against roughly a
dozen real Greenhouse/Lever/Ashby boards while building this never
actually hit one where HEAD and GET disagreed, so it's a defensive
measure, not something observed to be load-bearing against real traffic
yet) and, if it responds with any `2xx` status, upserts the
company and target board. A candidate that doesn't respond is skipped,
not persisted, and logged — a partial run (some candidates succeed, some
don't) exits `0`; `discover` only exits non-zero if every candidate
failed. `configs/seed_companies.json` ships with two real, working
examples (Spotify on Lever, Airbnb on Greenhouse — both verified live,
including the redirect chain the Greenhouse one goes through) as a
starting template.

Concurrency is bounded by `DISCOVERY_WORKERS`, independent of the
database pool or HTTP client's own connection pool.

### Search-based discovery

```bash
aggregator search-discover                          # uses configs/company_names.txt
aggregator search-discover path/to/custom_names.txt
```

Requires `SERPER_API_KEY` (see `.env.example` for the free,
no-card-required setup steps — https://serper.dev/, 2,500 free queries).
Given just a plain-text list of company names — one per line, `#` for
comments — this finds each company's ATS board via the Serper search
API instead of requiring a human to look up and type the full
`board_url`/`ats_provider`/`external_board_id` record `discover`'s seed
file needs. For each name it searches
`"<name>" (site:boards.greenhouse.io OR site:job-boards.greenhouse.io OR site:jobs.lever.co OR site:jobs.ashbyhq.com)`,
takes the first result whose URL matches a known ATS host pattern,
and extracts the provider + board slug from it (`internal/discovery/search.go`'s
`knownProviders` patterns) — deterministic pattern matching on the URL,
not an LLM parsing search results, since that's a solved lookup problem
that doesn't need one. The resulting candidates go through the exact
same `Discoverer.Run` pipeline `discover` uses: HEAD/GET validation,
then persistence. A company search can fail (credits exhausted, no
known board found in the results) without stopping the rest — same
partial-success-exits-0 policy as `discover`.

Searches run sequentially, not concurrently: Serper's free tier is a
fixed pool of query credits, not a throughput problem worth a worker
pool over. `configs/company_names.txt` ships with six names, all
live-verified through this exact command against the real Serper API
and a real Postgres database (0 failures) — add more names freely; a
default, no-argument run only searches this one file.

## Job API

```bash
aggregator serve   # http://localhost:8080/api/v1/jobs
```

Public, read-only JSON API backing a job-board frontend — full contract in
[docs/api.md](docs/api.md). Today it serves 12 illustrative fixture jobs from
`internal/job.MockRepository`, since Phase 3 (ATS ingestion) hasn't shipped a real
`jobs` table yet. `internal/api` depends only on the `job.Repository` interface, never
`MockRepository` directly — swapping in a real Postgres-backed implementation later is a
one-line change in `runServe` (`cmd/aggregator/main.go`), with no handler, routing, or
frontend change required.

CORS is a single explicit allowed origin (`CORS_ALLOWED_ORIGIN`, default matching Vite's
dev server), never a wildcard.

## Docker

```bash
docker compose up -d db      # PostgreSQL only, for local development/tests
docker compose up --build    # PostgreSQL + the aggregator binary
```

Postgres's port is published on `127.0.0.1` only. The `app` service waits
for the database's healthcheck before starting. It does not run
migrations automatically; run `docker compose run --rm app migrate-up`
once against a fresh database first. `configs/` ships inside the image
alongside the binary (not just the compiled binary alone), since
`discover`'s default seed file path is resolved relative to the
process's working directory.

If you're running from a git worktree (see CLAUDE.md's branching
workflow), Compose derives its project name from the current directory,
so `docker compose run --rm app ...` from a worktree tries to start its
*own* `db` service — which fails on a port collision if another
Postgres (from the main checkout, or another worktree) is already bound
to `127.0.0.1:5432`. Either stop the other one first, or run the whole
stack from the same checkout consistently.

## Architecture

Modular monolith, one deployable binary. Package boundaries so far:

- `internal/config` — env parsing and validation. No I/O beyond
  `os.Getenv`.
- `internal/observability` — `slog` construction.
- `internal/database` — the `pgxpool` connection pool and the migration
  runner. Nothing else in the codebase touches `pgxpool` directly.
- `internal/httpclient` — the one pooled, retrying HTTP client every
  outbound-network package shares (discovery today; ATS ingestion in a
  later phase). Knows nothing about job boards or providers.
- `internal/company` — domain types and the only code that writes to
  `companies`/`target_companies`. `Store`/`TargetStore.Upsert` are each a
  single atomic `INSERT ... ON CONFLICT`, never a check-then-insert.
- `internal/search` — wraps the Serper search API. Knows nothing about
  ATS providers either; returns raw `(title, URL, snippet)` results for
  a query string, same as any other search API client.
- `internal/discovery` — turns a list of candidates into persisted
  companies/targets via a bounded worker pool over `internal/company`'s
  repositories and `internal/httpclient`. Two sources feed it the same
  `Candidate` type: `LoadSeedCandidates` (a hand-curated JSON file) and
  `CandidatesFromSearch` (a plain list of company names, resolved to a
  board URL via `internal/search` and a set of known-ATS-host regex
  patterns). Depends on `company`, `httpclient`, and `search` through
  small consumer-defined interfaces (`CompanyUpserter`, `TargetUpserter`,
  `SearchClient`), not their concrete types, so its own orchestration
  logic (HEAD/GET fallback, concurrency bound, error propagation, URL
  pattern matching) is tested with fakes instead of a database or a real
  API key.
- `internal/job` — the normalized `Job` type and the `Repository`
  interface `internal/api` depends on. `MockRepository` (deterministic
  fixture data) is the only implementation today; a real Postgres-backed
  one arrives with Phase 3 and is a drop-in swap, not a rewrite.
- `internal/api` — the public, read-only job API (`serve`). Routing,
  JSON encoding, CORS, and request logging over `job.Repository` — never
  a concrete repository type. Full contract in [docs/api.md](docs/api.md).
- `migrations/` — versioned SQL, embedded via `go:embed`.
- `configs/` — operator-editable data files: `seed_companies.json` (full
  board records) and `company_names.txt` (just names, for
  `search-discover`).
- `cmd/aggregator` — composition root: wires config → logger → pool,
  dispatches `run`/`migrate-up`/`migrate-down`/`migrate-force`/
  `discover`/`search-discover`/`serve`, handles graceful shutdown.

`ats`, `ingestion`, `filtering`, `ranking`, `notification`, and
`scheduler` do not exist yet — they're created when the phase that needs
them lands, not before.

A separate frontend (React + Vite + TypeScript + Tailwind, a job-board
preview UI) lives in its own repository, `remote-job-aggregator-web`,
not in this one — it talks to `serve` over HTTP and has no other
coupling to this codebase.
