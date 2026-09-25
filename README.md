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
aggregator ingest [--providers=a,b]   # fetch jobs from the active targets (Greenhouse
                                       # boards, RSS feeds, careers pages), upsert them, and
                                       # close out jobs that disappeared, and collect the newest
                                       # jobs from Ethiojobs and six remote job boards (Himalayas,
                                       # Remotive, Jobicy, We Work Remotely, Working Nomads,
                                       # Remote OK; free, on by default; each board has a minimum
                                       # gap between runs, --force overrides it). The Serper-
                                       # backed sources spend a fixed query allowance, so they
                                       # never run by default: name them, --providers=search
                                       # (LinkedIn, per priority company) or --providers=linkedin
                                       # (LinkedIn, by keyword). See docs/fresh-jobs.md
aggregator seed-priority [file]       # register the Ethiopian priority companies and their
                                       # sources (default configs/ethiopian_companies.json)
aggregator priority-report            # per-company open jobs by source, and how many of the
                                       # priority companies have at least one open job
aggregator serve                      # start the public, read-only job API (see docs/api.md)
```

`migrate-down` always requires `--yes`: rolling back even one migration is
destructive. This repo has two migrations: a single step undoes only the
latest (`000002` drops the `companies.is_priority` column), and `--all`
undoes every one, dropping all tables.

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
| `SERPER_API_KEY` | no | — | for `search-discover` and the search-backed source of `ingest`; from https://serper.dev/api-keys |
| `SEARCH_MAX_QUERIES_PER_RUN` | no | `60` | cap on Serper queries one `ingest` run may spend across the `search` (1 per priority company) and `linkedin` (1 per keyword) sources, retries included; 1 – 2500. Serper's free tier is a fixed 2,500 queries, not a monthly allowance |
| `ETHIOJOBS_MAX_PAGES` | no | `100` | most listing pages (12 jobs each) one Ethiojobs collection reads; 1 – 200 |
| `HIMALAYAS_MAX_PAGES` | no | `50` | most pages (20 jobs each) one Himalayas collection reads; 1 – 100 |
| `API_ADDR` | no | `:8080` | only for `serve`; must be a valid `host:port` |
| `CORS_ALLOWED_ORIGIN` | no | `http://localhost:5173` | only for `serve`; a single explicit origin, never `*` |
| `JOB_REPOSITORY` | no | `postgres` | only for `serve`; `postgres` (real ingested jobs) or `mock` (12 fixture jobs, no database) |
| `JOB_MAX_AGE_DAYS` | no | `15` | only for `serve` (postgres repository); jobs posted longer ago than this are hidden from the list and detail (`0` = no limit); 0 – 3650. The rows stay in the database |
| `INGESTION_WORKERS` | no | `5` | bounded concurrency for `ingest` (one ATS board per worker); 1 – 100 |
| `INGESTION_MAX_RESPONSE_SIZE` | no | `33554432` (32 MiB) | bytes; `ingest`'s own response cap, separate from `HTTP_MAX_RESPONSE_SIZE` because big boards exceed 5 MiB (databricks: 9.7 MB); must be positive |
| `INGESTION_HTTP_TIMEOUT` | no | `30s` | whole-round-trip budget for `ingest`'s HTTP client; must be positive |
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
[docs/api.md](docs/api.md). By default (`JOB_REPOSITORY=postgres`) it serves the real jobs
`aggregator ingest` has stored — only `open` ones; jobs that disappeared from their board
are `removed` and never shown. `JOB_REPOSITORY=mock` serves 12 illustrative fixture jobs
from `internal/job.MockRepository` with no database, for frontend development.
`internal/api` depends only on the `job.Repository` interface, never a concrete type.

Typical local flow: `migrate-up`, `search-discover` (find boards), `ingest` (fetch jobs),
`serve`. `ingest` only has a client for Greenhouse today: targets on Lever or Ashby (which
`search-discover` also finds) are reported as `no ATS client registered` on every run until
those providers are added. If a board's ATS reports it gone (404), `ingest` deactivates the
target and closes its jobs; re-running discovery reactivates it. Every ingested job's `remote_type`/`employment_type` is `unknown` and `tags` is
empty — Greenhouse's public API has neither, and classifying them is a later phase's job.

CORS is a single explicit allowed origin (`CORS_ALLOWED_ORIGIN`, default matching Vite's
dev server), never a wildcard.

## Priority companies (Ethiopian tech)

Jobs from 25 curated Ethiopian tech companies are badged "Ethiopian company" (`is_priority`
in the API; `?priority=true` narrows a list to them). They sort like every other job, newest
first.
The list lives in `configs/ethiopian_companies.json`; how their jobs are found, what each
source checks before it trusts a result, and the measured coverage are in
[docs/priority-companies.md](docs/priority-companies.md).

```bash
aggregator migrate-up        # adds companies.is_priority
aggregator seed-priority     # registers the 25 companies and their sources (idempotent)
aggregator ingest                    # free sources, e.g. daily
aggregator ingest --providers=search # LinkedIn per priority company, 25 Serper queries, e.g. weekly
aggregator ingest --providers=linkedin # newest LinkedIn jobs by keyword, ~24 Serper queries
aggregator priority-report   # what is actually covered
```

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
- `internal/ats` — the provider-agnostic `ats.Job` shape every ATS client
  normalizes into, `ats.ErrBoardNotFound`, and `HTMLToText` (real HTML
  parsing via `golang.org/x/net/html`, not regex). `internal/ats/greenhouse`
  is the first provider (Greenhouse's public Job Board API, no key needed).
  Three more job sources share the same `ats.Job` shape: `internal/ats/feed`
  (RSS), `internal/ats/careers` (a company's own careers page) and
  `internal/ats/jobsearch` (web search over LinkedIn, with strict company
  verification). `internal/ats/ethiojobs` and the keyword-driven `FreshClient`
  in `jobsearch` are collectors (many employers per source, see
  `internal/ingestion/collectors.go` and docs/fresh-jobs.md). `internal/ats/page` is their shared
  robots-gated page fetcher and parser, and `internal/robots` the RFC 9309
  robots.txt checker every page fetch goes through.
- `internal/priority` — the curated priority-company list: loading and
  validating `configs/ethiopian_companies.json`, seeding companies/targets,
  and the read-only coverage report.
- `internal/job` — the normalized `Job` type, the `Repository` interface
  `internal/api` depends on, `Store` (real Postgres: upsert by
  `(source, source_job_id)` with content-hash change detection, and
  closing out jobs that vanished from a board), and `MockRepository`
  (fixture data for frontend development).
- `internal/ingestion` — bounded worker pool: one target board per worker,
  per-target error isolation; a board-not-found deactivates the target, a
  transient error never does, and a failed fetch never closes out jobs.
- `internal/api` — the public, read-only job API (`serve`). Routing,
  JSON encoding, CORS, and request logging over `job.Repository` — never
  a concrete repository type. Full contract in [docs/api.md](docs/api.md).
- `migrations/` — versioned SQL, embedded via `go:embed`.
- `configs/` — operator-editable data files: `seed_companies.json` (full
  board records), `company_names.txt` (just names, for
  `search-discover`) and `ethiopian_companies.json` (the priority list).
- `cmd/aggregator` — composition root: wires config → logger → pool,
  dispatches `run`/`migrate-up`/`migrate-down`/`migrate-force`/
  `discover`/`search-discover`/`seed-priority`/`priority-report`/`ingest`/
  `serve`, handles graceful shutdown.

`filtering`, `ranking`, `notification`, and
`scheduler` do not exist yet — they're created when the phase that needs
them lands, not before.

A separate frontend (React + Vite + TypeScript + Tailwind, a job-board
preview UI) lives in its own repository, `remote-job-aggregator-web`,
not in this one — it talks to `serve` over HTTP and has no other
coupling to this codebase.
