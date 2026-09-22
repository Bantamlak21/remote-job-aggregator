# Project Status

_Last updated: 2026-09-22._

## 1. Project Overview

**remote-job-aggregator** is an autonomous remote-job aggregation platform, built as a Go
modular monolith. Its purpose: discover, collect, normalize, filter, rank, store, and notify
about legitimate remote job postings — with particular attention to correct geographic
eligibility (default target: Ethiopia), reliable deduplication, and data quality over raw
volume — so a software engineer doesn't have to manually check hundreds of company career
pages.

Target scale (architectural, not yet exercised): 10,000+ companies, 100,000+ jobs, without
loading the full dataset into memory. Tech stack: Go (stdlib-first), PostgreSQL, Docker.
Full requirements live in the repo's (gitignored, not committed) `CLAUDE.md`.

**Phase 1 and Phase 2 are merged to `main`.** A search-based discovery mechanism (an
extension of Phase 2's discovery, addressing the explicit requirement to find companies via
a real web search instead of a hand-curated seed file) is implemented, reviewed, and — as of
this document — being committed/pushed with a PR opening; see Section 2's "Search-based
discovery" subsection. There is still no ATS integration, ingestion, filtering, ranking,
scheduling, or notification code: nothing in the system yet reads or stores an actual job
posting.

## 2. Completed Work

### Phase 1: Configuration, database pool, migrations, CLI, graceful shutdown

Merged to `main` via PR #1 (commit `7abc04d`, merge commit `beb25bf`, 2026-09-17).

**Config — `internal/config/config.go`**
- Loads and validates all operational config from environment variables (`config.Load()`).
- Explicit types (`Environment`, `LogFormat`, `DatabaseConfig`, `LogConfig`,
  `ShutdownConfig`, `HTTPConfig`, `DiscoveryConfig` — the last two added in Phase 2), no
  global mutable state — constructed once in `main` and passed down.
- Every validation failure is collected and returned together (`errors.Join`), not one at a
  time.
- `DATABASE_URL` validation never echoes the raw or even partially-parsed value in an error
  message — only the parsed URL scheme (`schemeOf`) — specifically because an earlier
  version leaked credentials into error text for malformed/scheme-less URLs.
- `DatabaseConfig.LogValue()` redacts credentials in both forms pgx/libpq accept: userinfo
  (`postgres://user:pass@...`) and a `?password=` query parameter.
- Pool size fields (`MaxConns`, `MinConns`) are `int32` at the type level (matching
  pgxpool's own field types) with range validation in `Load()`, not an unchecked cast later.
- Tests: `internal/config/config_test.go` — defaults, overrides, every validation failure
  path (including the two credential-leak shapes above), redaction through `LogValue()`.

**Database & migrations — `internal/database/`**
- `database.go`: wraps `pgxpool.Pool` (`New`, `Health`, `Close`). Pool sizing (max/min
  conns, conn lifetime/idle time) comes entirely from `config.DatabaseConfig`, explicit
  precedence over any `pool_max_conns`-style DSN query params.
- `migrate.go`: migration runner on top of `golang-migrate/migrate/v4` (pgx5 driver,
  embedded `iofs` source). Exposes `MigrateUp`, `MigrateDown` (full rollback),
  `MigrateDownStep` (roll back one migration), `MigrateForce` (clear a dirty
  `schema_migrations` state without running anything).
- `checkInterrupted(ctx)`: every migrate function checks `ctx.Err()` before returning
  success, because golang-migrate's own graceful-stop mechanism returns the same `nil` on a
  stop as on genuine completion — without this check, a SIGTERM mid-migration could report
  "applied"/"rolled back" and exit 0 having done partial or no work. Verified live against a
  real lock-blocked database.
- `watchGracefulStop`: wires `ctx` cancellation to golang-migrate's `GracefulStop` channel so
  Ctrl-C/SIGTERM stops a migration between files, not mid-statement.
- Tests: `internal/database/database_test.go` (20 tests) — pool connectivity, unreachable-host
  handling, migration idempotency (including a golang-migrate inconsistency where `Steps(-1)`
  at nil version returns `os.ErrNotExist` instead of `migrate.ErrNoChange`, disambiguated
  from a genuine "no migration found for version N" failure), context-cancellation handling
  for all three migrate functions, and schema-constraint enforcement (see below). All run
  against a **real PostgreSQL instance** via `docker-compose`, not mocks; skip cleanly
  (`t.Skip`) if `TEST_DATABASE_URL` is unset, and refuse to run at all unless that URL's
  database name ends in `_test` (these tests call `MigrateDown`, which drops every table).

**Schema — `migrations/000001_init_schema.{up,down}.sql`**
- Tables: `companies`, `target_companies`, `jobs`. See [Section 5](#5-current-database-state)
  for full detail.
- DB-level invariants (not just application logic): composite foreign keys tying a job's
  `company_id`/`source` to the target board it actually belongs to; non-blank `CHECK`
  constraints on every identity field; `status`/`closed_at` consistency;
  `last_seen_at >= first_seen_at`; case/whitespace-insensitive uniqueness on company name,
  company website (normalized), and target board id.
- `updated_at` maintained by a trigger that only fires on a genuine change (not a no-op
  `UPDATE`), not application code.
- Deliberately **no** `job_eligibility` table yet — nothing populates it until the filtering
  phase.

**Observability — `internal/observability/logger.go`**
- `slog` construction, JSON or text handler, configurable level.
- Tests: `internal/observability/logger_test.go` — level filtering, handler format, and
  credential redaction verified through a **real `slog.Handler`** (not just the
  `LogValue()` method directly — an earlier version's redaction bug only surfaced when
  tested this way).

**CLI — `cmd/aggregator/main.go`**
- Commands: `run`, `migrate-up`, `migrate-down` (always requires `--yes`; add `--all` for a
  full rollback), `migrate-force <version>` — plus `discover` added in Phase 2.
- Graceful shutdown: first SIGINT/SIGTERM cancels context; a second one forces immediate
  `os.Exit(1)` (`installSignalHandling`) — needed because golang-migrate's own lock
  acquisition ignores the context it's given and runs with `context.Background()`
  regardless, so a migration blocked on another process's lock is not interruptible by a
  first signal at all (verified live).
- Tests: `cmd/aggregator/main_test.go` — CLI dispatch/validation, `closeWithTimeout`
  behavior, double-signal force-exit (via subprocess re-exec, the standard Go idiom for
  testing `os.Exit`), signal-handler idempotency.

**Docker — `docker-compose.yml`, `Dockerfile`, `.dockerignore`, `scripts/postgres-initdb/`**
- `docker-compose.yml`: `db` (Postgres 17, port bound to `127.0.0.1` only) +
  `app` services. `scripts/postgres-initdb/01-create-test-db.sql` auto-provisions a second
  `aggregator_test` database so integration tests never point at the dev database.
- `Dockerfile`: multi-stage build, `-trimpath -ldflags="-s -w"`, `ca-certificates` installed
  in the final image, non-root user. **Updated in Phase 2** — see below.

**Review process.** Five rounds of independent, adversarial critic review, each verifying
claims by actually running commands, several with mutation testing (deliberately reverting a
fix to confirm its regression test fails without it). Closed a credential leak, missing
DB-level invariants, an unsafe `migrate-down` default, and a false-success-on-signal bug
before merge.

---

### Phase 2: Company persistence, HTTP client, discovery

**Status: complete, merged to `main` via PR #3 (commit `e47e17d`, merge commit `d84bab9`,
2026-09-22).** No schema change (no new migration): Phase 2 is application code over the
tables Phase 1 already created.

**`internal/httpclient`** — the one pooled, retrying HTTP client every outbound-network
package shares (discovery today; ATS ingestion in a later phase).
- `Config`/`DefaultConfig()`/`New(cfg) *Client`/`(*Client).Do`. `New` builds its own
  `*http.Transport` (never `http.DefaultTransport`) with explicit `MaxIdleConns`,
  `MaxIdleConnsPerHost`, `IdleConnTimeout`; TLS verification untouched.
- `Do` retries 429 and 5xx (never other 4xx) and network errors, up to `MaxRetries`, with
  exponential backoff + jitter; honors a response's `Retry-After` header (seconds or
  HTTP-date) over the computed backoff, uncapped — a deliberate choice (a caller without a
  context deadline can stall on a server's arbitrarily long `Retry-After`; the responsibility
  is placed on the caller supplying a bounded context, not the client silently ignoring what
  the server asked for).
- Enforces `MaxResponseBytes` on the final response body via a wrapper that reads through
  `io.LimitReader(body, limit+1)`: an exactly-at-limit body reads cleanly to `io.EOF`, an
  over-limit one returns `ErrResponseTooLarge` (sticky on subsequent reads) instead of
  silently truncating.
- Redirect cap via `CheckRedirect`. **Found and fixed during review:** the original
  `len(via) >= cfg.MaxRedirects` check was off by one — since `via` includes the original
  request, it only permitted `MaxRedirects - 1` actual redirects, so `MaxRedirects: 1`
  behaved identically to `MaxRedirects: 0` (refused every redirect). Fixed to
  `len(via) > cfg.MaxRedirects`; regression tests pin both the exactly-one-redirect case and
  the zero-redirects case.
- Tests: `internal/httpclient/httpclient_test.go` (29 tests) — retry policy across ~20 status
  codes, `Retry-After` parsing, the exact-boundary size-limit behavior, context cancellation
  during a queued backoff, redirect-cap boundaries. All mutation-verified during review
  (deliberately-broken variants confirmed to fail their matching test).

**`internal/company`** — domain types and the only code that writes to
`companies`/`target_companies`.
- `Store`/`TargetStore`, each with `Upsert` as a single atomic `INSERT ... ON CONFLICT`
  (never check-then-insert) targeting the exact expression the real unique index is built
  on — verified against live Postgres via `EXPLAIN` showing the real conflict arbiter index,
  plus a negative control (a subtly-wrong conflict target raises a real Postgres error,
  proving inference is strict).
- `Store.Upsert`: fills a NULL `website`/`description` on conflict, never overwrites an
  existing non-null value. `TargetStore.Upsert`: `is_active` always reactivated to `true`
  (re-discovery wins over a previous deactivation — there's no operator UI to deactivate a
  target on purpose yet); `board_url` takes the freshly-probed value over a stale one;
  `discovery_metadata` replaces wholesale when explicitly supplied, otherwise left alone
  (tri-state via a nil vs. non-nil map).
- `ErrNotFound` translated from `pgx.ErrNoRows`, never leaked raw. All SQL parameterized.
- Tests: `internal/company/company_test.go` + `target_test.go` (27 tests) against real
  Postgres, including `TestStoreUpsert_ConcurrentCallersConvergeOnOneRow` (8 concurrent
  callers, one row, one ID — the actual proof `ON CONFLICT` is doing the work a
  check-then-insert couldn't).

**`internal/discovery`** — turns a list of candidate ATS boards into persisted
companies/targets.
- `LoadSeedCandidates(path)`: reads a JSON array, validates every candidate before returning
  any (one bad entry fails the whole load with every invalid candidate's error reported
  together via `errors.Join`, not just the first).
- `Discoverer.Run`: bounded worker pool (`DISCOVERY_WORKERS`, config-validated to 1–100).
  Depends on `company`/`httpclient` through small consumer-defined interfaces
  (`CompanyUpserter`, `TargetUpserter`), not their concrete types, so orchestration logic is
  tested with fakes rather than a database.
- `probe`: HEAD first, falling back to GET on any non-2xx or network error from HEAD — per
  CLAUDE.md's explicit rule that some ATS-hosted boards don't handle HEAD reliably. The
  mechanism is proven against a controlled fake server
  (`TestRun_HeadFailsGetSucceeds_FallsBackToGet`); live spot-checks against ~44 real
  Greenhouse/Lever/Ashby/other ATS boards during review never found one where HEAD and GET
  actually disagreed in the 2xx/non-2xx sense the code acts on — so the fallback is
  defensive per the stated requirement, not something observed to be load-bearing against
  real traffic yet (this distinction matters: an earlier draft claimed it was "verified
  live," which review found unsupported and corrected).
- **Found and fixed during review:** `New` with `workers <= 0` spawned zero worker
  goroutines, so `Run`'s feeder goroutine leaked forever (blocked on an unbuffered channel
  send nothing would ever drain) on any non-empty candidate list — reproduced with a
  goroutine stack dump. Unreachable from the CLI (config already validates workers ≥ 1
  before `New` is called) but `New` is exported, so it now clamps `workers` to at least 1.
- Tests: `internal/discovery/discovery_test.go` + `seed_test.go` (23 tests) — HEAD/GET
  fallback, worker-bound enforcement (measured, not assumed), invalid-candidate rejection
  before any HTTP call, context-cancellation handling, the workers-clamp regression.

**CLI — `aggregator discover [seed-file]`**
- Defaults to `configs/seed_companies.json`. Loads candidates, connects to the database,
  runs discovery, logs a per-candidate outcome, and exits non-zero only if *every* candidate
  failed — a mix of successes and failures is discovery's normal steady state and must not
  fail a cron-style invocation.
- An empty candidate list (`[]`) returns success without ever connecting to the database
  (verified: run against a deliberately unreachable `DATABASE_URL`, confirmed no connection
  error).
- Tests: argument validation, missing/empty seed file behavior — plus a full live run
  against a real migrated database and the real seed file (see below).

**`configs/seed_companies.json`** — ships two real, live-verified examples: Spotify on Lever
(`https://jobs.lever.co/spotify`, direct 200) and Airbnb on Greenhouse
(`https://boards.greenhouse.io/airbnb`, a 2-hop redirect chain to a 200). Both were run
against the real internet during development and again during both review rounds, not just
asserted to work.

**Docker — found and fixed during review.** The Dockerfile's final stage originally copied
only the compiled binary, not `configs/` — so `discover`'s default seed file path didn't
exist inside the container. Fixed: final stage now has `WORKDIR /app`, copies `configs/`
alongside the binary, `chown`s both to the non-root user. Verified: built the image, ran
`docker compose run --rm app discover` against the containerized database, confirmed both
seed companies discovered from inside the container using the container's own outbound
network.

**Config additions** — `HTTP_TIMEOUT`, `HTTP_MAX_RESPONSE_SIZE`, `HTTP_USER_AGENT`,
`DISCOVERY_WORKERS`, validated the same way every other `internal/config` field is (defaults,
override tests, invalid-value rejection with specific messages, all failures reported
together).

**Cross-package test collision — found and fixed during review.** With two packages
(`internal/database`, `internal/company`) now resetting the shared `aggregator_test`
database's schema in their own tests, `go test ./...`'s default package-level parallelism
runs their test binaries as separate concurrent processes with no interlock — they drop each
other's tables mid-run. Reproduced 3/3 tries without `-p 1`, clean 3/3 with it. Fixed via new
`make test-integration`/`test-integration-race` Makefile targets that always pass `-p 1`, and
a README warning that a bare `go test ./...` with `TEST_DATABASE_URL` set does not and will
intermittently fail. **This is a convention, not a mechanism** — see Section 8.

**Review process.** Built in three pieces: `internal/httpclient` and `internal/company` were
each built by an independent subagent in an isolated worktree (both delivered
mutation-tested); `internal/discovery`, CLI wiring, the Docker fix, and config extension were
built directly. The integrated whole then went through two rounds of independent adversarial
review. Round 1 (REJECT): found the three "found and fixed during review" items described
above under `httpclient` and `discovery`. Round 2 (ACCEPT): independently re-verified all
three fixes — 44 live ATS URL probes for the HEAD/GET claim, a from-scratch 10-case redirect
budget matrix proving both the bug and the fix, and a clamp-reverted goroutine-leak
reproduction by stack name — plus three non-blocking cosmetic observations, which were also
addressed (a test name that only checked a lower bound, a missing explicit
`MaxRedirects=0` test, a misleading test failure message).

---

### Search-based discovery (extends Phase 2, this session)

**Status: complete, reviewed (two rounds of independent adversarial critic review, round 2
ACCEPT), being committed/pushed with a PR opening as of this document.** No schema
migration; one query-level behavior change to an existing table (see below).

**Why this exists:** the user explicitly redirected discovery away from hand-curated seed
data — "Instead of seeding companies, or faking it, it's better to work with a real search."
— toward a real web search API. Resolved to the Google Custom Search JSON API (the only
option that is both genuinely free and requires no credit card, per the user's own
constraint: "Any API as long as it is free and real").

**`internal/search`** (new package) — wraps the Google Custom Search JSON API.
- `Client.Search(ctx, query) ([]Result, error)`. Config is `APIKey` + `SearchEngineID`.
- The API key travels as the `X-goog-api-key` HTTP header, never as a URL query parameter —
  found during round-1 adversarial review that a query-string key leaks through Go's own
  `*url.Error` on network failures/timeouts (the full request URL, query string included, is
  embedded in that error's string before any `%w` wrapping ever sees it — no downstream
  redaction can close this once the key is in the URL). Verified against Google's own Cloud
  documentation, which recommends the header form for exactly this reason.
- Quota-exceeded detection (`ErrQuotaExceeded`) recognizes both the newer
  `status: "RESOURCE_EXHAUSTED"` shape and the classic `errors[].reason` shape
  (`dailyLimitExceeded`, etc.) — the classic shape was a gap found in round-1 review.
- Tests: `internal/search/google_test.go` (10 tests) — result parsing, required query
  params, the header-not-URL key placement, a real network-error probe against a
  nothing-listens-here address confirming the key never appears in the resulting error
  string, both quota-exceeded shapes, non-JSON error bodies, context cancellation.
- **Not yet verified against the real googleapis.com endpoint** — no
  `GOOGLE_SEARCH_API_KEY`/`GOOGLE_SEARCH_ENGINE_ID` exists in this environment. All
  verification so far is against fake `httptest` servers. This is a named, outstanding gap —
  see Section 8.

**`internal/discovery/search.go`** (new file) — turns company names into `Candidate`s by
searching and recognizing a known ATS board URL (Greenhouse, Lever, Ashby) in the results,
via regex — deterministic pattern matching, never an LLM call, per CLAUDE.md's LLM-usage
rule (this is a same-input-same-output extraction problem).
- `buildSearchQuery`: quotes the company name, restricts via `site:` to the three known ATS
  hosts, and strips `"`/newlines from the name first — closing a low-severity
  query-injection vector found in round-1 review (an operator-supplied name containing a
  literal `"` could otherwise break out of the quoted phrase and inject extra `OR`/`site:`
  terms).
- The three URL-recognizing regexes are case-insensitive on scheme/host (hosts are
  case-insensitive per RFC — a bare case-sensitive match missed real URLs, found in review)
  and boundary-anchored after the slug (`(?:[/?#]|$)`) so a URL like
  `boards.greenhouse.io/acme.inc` is rejected outright rather than silently truncated to the
  wrong slug `acme` — also found in review.
- `greenhouseReservedSlugs` rejects Greenhouse's own `embed` path segment
  (`boards.greenhouse.io/embed/job_board?for=<company>`, a real, commonly-indexed
  embeddable-widget URL) from ever being treated as a company's board slug — this was
  round-1's first BLOCKER: without it, two different companies whose search results both
  surfaced an embed URL would collide on the same fake "embed" slug and corrupt each other's
  `target_companies` row.
- `CandidatesFromSearch` runs searches sequentially, not concurrently — Google's free tier is
  a 100-queries/day budget, not a throughput problem worth a worker pool over. One company's
  search failure never stops the batch; every name is attempted and every per-name error
  collected.
- Tests: `internal/discovery/search_test.go` — provider recognition (all three ATS hosts),
  the embed-URL rejection (plus proof a real board is still found when an embed URL is also
  present in the same result set), case-insensitive host matching, the boundary-anchor
  truncation rejection, the nonexistent-`www.`-subdomain rejection, query sanitization,
  per-company error isolation, context cancellation.

**`internal/discovery/names.go`** (new file) — `LoadCompanyNames(path)` reads a plain-text,
newline-delimited company-name list (`configs/company_names.txt` by default): blank lines
and `#`-comments ignored, case-insensitive dedup (first spelling wins). Tests:
`internal/discovery/names_test.go` — valid file, comments/blanks, dedup, missing file, empty
file (→ `ErrNoCompanyNames`).

**`internal/company/target.go` — round-1 BLOCKER 2's repository-level fix.** The embed-slug
collision above is one way two companies could end up claiming the same
`(ats_provider, external_board_id)`; the regex-level fix above closes that one specific
trigger, but the repository itself had no defense against the general case. Added a
`WHERE target_companies.company_id = EXCLUDED.company_id` guard to `TargetStore.Upsert`'s
`ON CONFLICT DO UPDATE`, so a conflicting board already owned by a *different* company is
never silently reassigned. Detected via `pgx.ErrNoRows` (empirically verified against live
Postgres: `RETURNING` on a `WHERE`-guard-blocked `ON CONFLICT DO UPDATE` produces zero rows,
not the existing row's values), surfaced as the new `ErrTargetCompanyMismatch`. Added
`TargetStore.GetByProviderAndBoard` to build that error's "already registered to company N"
message. Tests (against real Postgres): the reassignment attempt is rejected and the
original row's `company_id`/`board_url` are provably unchanged; a legitimate same-company
re-upsert still succeeds; the new lookup method's found/not-found paths.

**`internal/config`** — added `SearchConfig` (`GoogleAPIKey`, `GoogleSearchEngineID`,
`Configured()`, redacting `LogValue()`). `Load()` requires both env vars set together or
neither (`GOOGLE_SEARCH_API_KEY`, `GOOGLE_SEARCH_ENGINE_ID`). Round-1 review flagged that the
config-level redaction test only called `.LogValue().String()` directly, never proving
redaction survives real `slog` attribute resolution — fixed with
`TestNewLogger_RedactsSearchConfigCredentials` in `internal/observability/logger_test.go`
(mirrors the existing `DatabaseConfig` version), and `cfg.Search` is now actually logged in
`runSearchDiscover` so `LogValue()` is exercised in real code, not just tests.

**CLI — `aggregator search-discover [company-names-file]`** — loads company names, searches
each, validates+persists any board found via the same `Discoverer.Run` pipeline
`discover` uses (HEAD/GET probing, then persistence) — "how a candidate was found" (seed
file vs. search) is fully decoupled from "what happens once we have one." Requires
`cfg.Search.Configured()`; fails fast, before touching the database, if company names can't
be loaded or no board is found for any of them. Tests:
`TestRunSearchDiscover_RejectsTooManyArgs`, `_RequiresSearchCredentials`,
`_MissingNamesFileFailsBeforeTouchingTheDatabase`.

**`configs/company_names.txt`** (new) — two example names (Spotify, Airbnb), explicitly
documented in the file's own header comment as **unverified against the real search API**
(unlike `configs/seed_companies.json`'s live-verified examples) — no Google API credentials
exist in this environment to run them for real.

**Review process.** Two rounds of independent adversarial critic review, following
CLAUDE.md's fan-out + harsh-critic loop for large work. Round 1 (REJECT): found the two
blockers and five should-fix items described above. Round 2 (ACCEPT, cold, no access to the
builder's reasoning): independently re-verified every fix — including writing standalone
scratch Go regex probes to adversarially hunt for a boundary-anchor bypass (tried
`acme.inc`, `acme%2e`, a trailing dot, a port number, embedded control characters,
prefix/suffix host-injection attempts — found none), tracing the actual leak path in
`internal/httpclient`'s retry-exhausted error to confirm the original vulnerability was real
and that moving the key out of the URL (not just redacting `req.URL`) is what actually closes
it, and running the full test suite including `internal/company`'s DB-guard test against a
real Postgres instance.

## 3. Work In Progress

Nothing is mid-implementation in the codebase itself. The search-based discovery work above
has passed round-2 review and is being committed/pushed with a PR opening in the same
session that finished this document. `git status` in this worktree, prior to that commit,
shows exactly the files listed in the "Search-based discovery" subsection above as modified
or new.

**The original shared checkout** at `/home/bantamlak/my-repos/remote-job-aggregator` (as
distinct from the worktree this document was written in) had, as of Phase 2's writing, two
files with uncommitted local modifications not made by any session:

- `.gitignore` — a `.claude/` ignore entry not present in the committed version.
- `Dockerfile` — cosmetic blank-line reformatting only.

Not re-checked this session; not touched by this session either way.

A Postgres container (`remote-job-aggregator-db-1`) has been running continuously since
before this document was first written and was used, read-only with respect to its role as
shared test infrastructure, throughout Phase 2's and this session's development and review.
Its `aggregator_test` database is clean after every test run (each test cleans up after
itself).

## 4. Remaining Implementation Roadmap

Ordered by dependency, per the original project requirements. Phase 2 (and the search-based
discovery extension to it, this session) is complete — see Section 2 — and removed from this
list. "Phase 3" below is the original roadmap's ATS-integration phase; it is unrelated to
and not renumbered by the search-discovery work, which was an extension of Phase 2's
discovery mechanism, not a new phase.

### Phase 3 — ATS integration & ingestion
- **Objective:** fetch and normalize jobs from Greenhouse (first provider), run ingestion
  as a bounded worker pool.
- **Tasks:** `ats` package with a `Provider`-scoped client interface and Greenhouse's
  first; provider-specific response models kept separate from the normalized `job.Post`
  type; `ingestion` package (load active targets from `internal/company`, bounded worker
  pool, decode/normalize, route through filtering/ranking once those exist); `job` package
  for normalization and lifecycle (new/changed/removed detection, content-hash comparison).
- **Expected files/modules:** `internal/ats/greenhouse/`, `internal/ingestion/`,
  `internal/job/`.
- **Dependencies:** Phase 2 (done — targets to ingest from via `internal/company`, the
  shared HTTP client via `internal/httpclient`).
- **Verification:** fake ATS test servers/fixtures (success, 404, 429, 500, invalid JSON,
  slow/large response, duplicate/changed jobs); `go test -race` for the worker pool. Note
  from Phase 2's review: verify any "handles X unreliably" claim about a *specific* real ATS
  behavior against the real internet before asserting it, the same way discovery's HEAD/GET
  fallback claim was checked and corrected.

### Phase 4 — Geographic eligibility & relevance filtering
- **Objective:** deterministic classification of remote-eligibility and role relevance.
- **Tasks:** `filtering` package — geographic classifier (ELIGIBLE/INELIGIBLE/UNCERTAIN
  with confidence, reasons, evidence, detected locations, restrictions) and relevance
  filtering (configurable positive/negative signals, not hard-coded to one job title). New
  migration adding `job_eligibility` (deferred from Phase 1 deliberately — see Section 5).
- **Expected files/modules:** `internal/filtering/`, `migrations/00000X_job_eligibility.*.sql`.
- **Dependencies:** Phase 3 (jobs to classify).
- **Verification:** unit tests are the primary tool (deterministic logic), plus an eval
  suite per CLAUDE.md's rule that classification quality needs more than deterministic
  tests alone.

### Phase 5 — Ranking & upsert/change detection
- **Objective:** deterministic, explainable scoring; wire filtering+ranking into ingestion
  with PostgreSQL upsert and lifecycle (new/changed/removed) detection.
- **Tasks:** `ranking` package (configurable scoring, explainable output); ingestion
  updated to upsert via `(source, source_job_id)` identity, detect changes via
  `content_hash`, mark removed jobs via `status`.
- **Expected files/modules:** `internal/ranking/`; changes to `internal/ingestion/`.
- **Dependencies:** Phase 4 (jobs need eligibility/relevance data to rank against).
- **Verification:** unit tests for scoring; integration tests for upsert/change-detection
  against real Postgres.

### Phase 6 — Scheduling & notifications
- **Objective:** periodic, unattended execution; notify on relevant new jobs.
- **Tasks:** `scheduler` package (periodic discovery/ingestion/cleanup, no distributed
  scheduling infra); `notification` package with a `Notifier` interface, Telegram first.
- **Expected files/modules:** `internal/scheduler/`, `internal/notification/telegram/`.
- **Dependencies:** Phase 5 (ranked jobs to notify about).
- **Verification:** scheduler tests with a fake clock/ticker; notifier tests against a fake
  Telegram endpoint.

### Phase 7 — Additional providers & observability
- **Objective:** Lever and Ashby support; metrics beyond structured logging.
- **Tasks:** `internal/ats/lever/`, `internal/ats/ashby/`; metrics
  (`jobs_fetched_total`, `provider_errors_total`, etc.) per CLAUDE.md §23.
- **Dependencies:** Phase 3's `ats` abstraction must already support a second/third
  provider without rework.
- **Verification:** same fake-server pattern as Greenhouse.

## 5. Current Database State

**Migrations:** one — `000001_init_schema` (up + down), applied and clean
(`schema_migrations`: version 1, dirty=false). Phase 2 added no migration; it only added Go
code (`internal/company`) over the tables Phase 1 already created.

**Tables:**

| Table | Purpose | Key constraints |
|---|---|---|
| `companies` | Organizations known to the system | PK `id`; `CHECK` non-blank `name`; unique on `lower(btrim(name))`; unique on `normalize_website(website)` where present (case-fold + trailing-slash strip, does **not** normalize http/https or www) |
| `target_companies` | A company's ATS boards (many per company) | PK `id`; FK `company_id → companies(id) ON DELETE CASCADE`; unique on `(ats_provider, lower(btrim(external_board_id)))`; `CHECK` non-blank `ats_provider`/`external_board_id`; two support-only unique constraints `(id, company_id)` and `(id, ats_provider)` used by `jobs`' composite FKs |
| `jobs` | Normalized postings | PK `id`; identity = unique `(source, source_job_id)` — **not** company+title; composite FKs `(target_company_id, company_id) → target_companies(id, company_id)` and `(target_company_id, source) → target_companies(id, ats_provider)` (both `ON DELETE CASCADE`) so a job can't be attributed to a company that doesn't own its board, and `source` can't drift from the board's own provider; `CHECK` non-blank on `source`/`source_job_id`/`title`/`application_url`/`content_hash`; `CHECK` `canonical_url IS NULL OR` non-blank; `remote_type`/`employment_type`/`status` are `CHECK`-constrained enums; `CHECK last_seen_at >= first_seen_at`; `CHECK (status IN ('closed','removed')) = (closed_at IS NOT NULL)`; unique partial index on `canonical_url` where present |

**Relationships:** `companies` 1—N `target_companies` 1—N `jobs`, with `jobs` also holding a
direct (constrained, see above) `company_id`. `companies`/`target_companies` are now
actively read and written by `internal/company`'s repositories (exercised live by `discover`
against real data — 2 companies, 2 targets, as of the last live verification run, cleaned up
afterward). `jobs` remains untouched by any code — nothing writes to it until Phase 3.

**Indexes beyond the uniqueness ones above:** `target_companies(company_id)`,
`target_companies(is_active)` (partial, active only), `jobs(company_id)`,
`jobs(target_company_id)`, `jobs(status)` (partial, `status='open'` only — the dominant
query pattern), `jobs(last_seen_at)`.

**`updated_at` triggers** on all three tables via a shared `set_updated_at()` function,
firing only when a column other than `updated_at` itself actually changed.

**Missing schema components (intentional, not oversights):**
- `job_eligibility` — geographic/relevance classification data. Belongs to Phase 4.
- No indexes/tables for ranking, scheduling state, or notification dedup — belong to
  Phases 5–6.

**Database-related pending work:** none requiring a migration. This session changed
`target_companies` upsert *behavior* (not schema): `TargetStore.Upsert`'s
`ON CONFLICT DO UPDATE` now carries a `WHERE target_companies.company_id =
EXCLUDED.company_id` guard, refusing to reassign a board already owned by a different
company (see Section 2's "Search-based discovery" subsection). The next schema change is
still the Phase 4 `job_eligibility` migration.

## 6. Current Architecture

**Implemented today:**

```
cmd/aggregator (composition root)
    │
    ├─ internal/config          (env → validated Config, incl. SearchConfig)
    ├─ internal/observability   (slog construction)
    ├─ internal/database        (pgxpool + migration runner)
    │       │
    │       └─ migrations/      (embedded SQL, go:embed)
    ├─ internal/httpclient      (pooled, retrying HTTP client)
    ├─ internal/search          (Google Custom Search JSON API client)
    ├─ internal/company         (Store/TargetStore over companies/target_companies)
    ├─ internal/discovery       (seed OR search → candidates → probe → persist)
    └─ configs/                 (seed_companies.json, company_names.txt)
```

One deployable binary. `cmd/aggregator/main.go` is the only place that wires concrete
implementations together. Nothing outside `internal/database` touches `pgxpool` directly;
nothing outside `internal/config` reads `os.Getenv`; nothing outside `internal/search` talks
to the Google Custom Search API. `internal/discovery` depends on `internal/company`,
`internal/httpclient`, and (for the search path) `internal/search` through small interfaces
it defines itself (`CompanyUpserter`, `TargetUpserter`, `SearchClient`) rather than their
concrete types — the consumer-defines-the-interface Go convention, chosen so discovery's own
orchestration logic (HEAD/GET fallback, concurrency bound, error propagation, board-URL
recognition) is unit-testable with fakes instead of always needing a database or a live
Google API key. Two independent ways to produce a `Candidate` (`LoadSeedCandidates` from a
JSON file, `CandidatesFromSearch` from a search) converge on the same `Discoverer.Run`
pipeline — "how a candidate was found" is fully decoupled from "what happens once we have
one."

**Planned, not implemented:** `internal/ats`, `internal/ingestion`, `internal/job`,
`internal/filtering`, `internal/ranking`, `internal/notification`, `internal/scheduler` — see
Section 4. These packages do not exist on disk. Do not assume any of their functionality when
reasoning about what the system currently does.

**Concurrency implemented today:** `pgxpool`'s internal connection management (bounded by
config); the CLI's signal-handling goroutines (`installSignalHandling`, `watchGracefulStop`,
`closeWithTimeout`); `internal/discovery`'s bounded worker pool
(`DISCOVERY_WORKERS`, independent of the database pool and the HTTP client's own connection
pool). No ingestion pipeline yet — that arrives in Phase 3.

## 7. Testing and Verification Status

**Run and passing as of this document (2026-09-22, this worktree, search-discovery work not
yet committed):**

```bash
gofmt -l .                             # clean
go build ./...                         # clean
go vet ./...                           # clean
TEST_DATABASE_URL=postgres://aggregator:aggregator@localhost:5432/aggregator_test \
  go test -race -p 1 -count=1 ./...    # all 9 packages ok, real Postgres, race detector —
                                        #   20 internal/database, 31 internal/company,
                                        #   29 internal/httpclient, 37 internal/discovery,
                                        #   27 internal/config, 10 internal/search,
                                        #   8 internal/observability, 19 cmd/aggregator
```

`internal/search` and the new tests in `internal/discovery`/`internal/company`/
`internal/config`/`internal/observability`/`cmd/aggregator` are new/changed this session.
All verified passing individually and as part of the full-suite run above; zero regressions
in any pre-existing test.

**`-p 1` is required** whenever `TEST_DATABASE_URL` is set and more than one package's tests
run together: `internal/database` and `internal/company` both reset the shared
`aggregator_test` schema, and `go test`'s default package-level parallelism runs them as
separate processes with no interlock — confirmed to collide 3/3 tries without `-p 1`, clean
3/3 with it. `make test-integration` / `make test-integration-race` already pass it.

**Recommended verification commands for a fresh session:**

```bash
# From a worktree (never the shared checkout — see Section 10):
gofmt -l .
go build ./...
go vet ./...
go test ./...                          # unit tests, no DB required

docker compose up -d db                # or reuse the already-running container if present
make test-integration
make test-integration-race

docker compose build app
docker compose run --rm app migrate-up
docker compose run --rm app discover   # confirms the full Docker + discovery path end to end;
                                        #   note the compose-project-name port-collision caveat
                                        #   in README.md's Docker section if running from a
                                        #   worktree alongside another running Postgres
```

**Live (non-unit-test) verification performed during Phase 2 development and both review
rounds:** the actual CLI binary run against a real, migrated Postgres and the real
`configs/seed_companies.json`, both from the host and from inside the built Docker container,
confirming real rows persisted with correct `discovery_metadata`; ~44 real ATS board URLs
probed directly with `curl` (Greenhouse, Lever, Ashby, and a few others) to check the HEAD/GET
fallback claim; a from-scratch redirect-budget test matrix against `httptest` servers,
independent of the checked-in test suite, to verify the `MaxRedirects` off-by-one fix.

**Missing tests:** none within Phase 1, Phase 2, or the search-discovery extension's own
scope — every behavior change in this session shipped with a regression test, per CLAUDE.md.
`internal/search/google.go` has not been tested against the real, live Google API (see
Section 8) — only against fake `httptest` servers; that gap is documented as such, not
papered over with an unverified claim. Coverage will grow with each future phase (fake ATS
servers for Phase 3, an eval suite for Phase 4's classifier, etc.) — none of that exists yet
because none of that code exists yet.

**Known failures:** none.

## 8. Known Issues and Risks

- **`httpclient`'s `Retry-After` handling is uncapped.** A response's `Retry-After` header is
  honored verbatim, however long it says. This is deliberate (servers that ask a client to
  wait get exactly that, not a client second-guessing them), but it means a caller that
  doesn't pass a context with its own deadline can stall for however long a
  misconfigured or hostile server's `Retry-After` says. Every current caller (`discover`)
  goes through the CLI's own context, which is bounded by process lifetime and the
  double-signal force-exit, not a per-request deadline — worth revisiting when Phase 3 adds
  a caller that might want a tighter bound.
- **The `-p 1` integration-test requirement is a convention, not a mechanism.** Nothing
  stops a future session (or a CI config) from running a bare `go test ./...` with
  `TEST_DATABASE_URL` set and silently corrupting the shared test database mid-run. A
  Postgres advisory lock in the shared test helper, or giving each DB-touching package its
  own schema/database, would make this impossible instead of merely documented. Not done
  yet — flagged during Phase 2 review as worth a follow-up, not a blocker.
- **`ats_provider` is an unconstrained `TEXT` column** (no DB-level `CHECK`/enum) by
  deliberate design — new providers shouldn't need a migration — so the Go-side
  `ats.Provider` type (which doesn't exist yet, arrives in Phase 3) is the only thing that
  will ever validate it. Not a bug today; `internal/company`'s repositories don't validate
  it beyond non-blank either, by the same reasoning.
- **Single migration file.** `migrate-down`'s `--all` vs. single-step distinction is
  currently untested in the "more than one migration" case, since there's only ever been
  one. This will get real exercise the moment Phase 4 adds a second migration.
- **Uncommitted changes in the shared checkout** (Section 3) — low-cost, not touched by this
  session, unrelated to Phase 2's own correctness.
- **`configs/seed_companies.json`'s two examples are companies' real, currently-live ATS
  boards.** If either Spotify or Airbnb ever changes ATS providers or takes their board down,
  the shipped example will start failing `discover` (gracefully — it logs and skips, doesn't
  crash) even though nothing in this repo is wrong. Worth a periodic manual re-check, not
  worth building automation around yet.
- **`internal/search` has never been run against the real Google Custom Search API.** No
  `GOOGLE_SEARCH_API_KEY`/`GOOGLE_SEARCH_ENGINE_ID` exists in this environment — these must
  come from Bantamlak (Google Cloud Console → enable Custom Search API → API key;
  Programmable Search Engine console → new engine → "Search the entire web" → the `cx` id;
  both free, no card required — see `.env.example` and README.md's Discovery section for the
  exact steps). Everything in this session was verified against fake `httptest` servers,
  honestly documented as such. **Next session with real credentials should run
  `search-discover` end to end** (e.g. against `configs/company_names.txt`'s two example
  names) and confirm: the header-based auth is actually accepted by the real endpoint, the
  response shape matches `apiResponse`'s assumptions, and — if it can be provoked without
  burning the whole daily quota — that the classic `dailyLimitExceeded` error shape
  (`quotaExceeded` in `internal/search/google.go`) is what Custom Search specifically
  returns, which round-2 review confirmed is documented for the API family but not yet
  observed from Custom Search itself.
- **`configs/company_names.txt`'s two example names (Spotify, Airbnb) are unverified against
  the real search API**, unlike `configs/seed_companies.json`'s live-verified examples — see
  above. The file's own header comment says so.

## 9. Exact Next Step

**Two independent next steps — neither blocks the other:**

1. **External, needs Bantamlak:** obtain real `GOOGLE_SEARCH_API_KEY`/
   `GOOGLE_SEARCH_ENGINE_ID` and run `search-discover` end to end against the real Google
   API — see Section 8's `internal/search` known-issue entry for exactly what that
   verification should check. This does not block Phase 3 and can happen whenever
   credentials become available.
2. **Development, no external dependency: start Phase 3** — build the `internal/ats`
   package with a Greenhouse client, and `internal/job` for the normalized job type.

Why this and not `internal/ingestion` first: ingestion's job is to orchestrate "for each
active target, fetch its jobs, normalize them, persist them" — it needs both something that
knows how to fetch+parse a specific ATS's response format (`internal/ats`) and a normalized
`job.Post` type with the identity/lifecycle rules from CLAUDE.md §9 to persist into (backed by
a new `internal/job` repository over the existing `jobs` table, following the exact same
`Store`/`Upsert`-via-`ON-CONFLICT` pattern `internal/company` already established and proved
out). Building the ATS client and the job type first, each independently testable against
fixtures/fakes, means ingestion's own tests can focus on orchestration (worker pool, error
isolation between targets, change detection) rather than re-deriving Greenhouse's response
shape or job identity rules inline.

Concretely: read CLAUDE.md §10 (ATS Integrations), §9 (Job Identity and Deduplication), and
§20 (Job Lifecycle) again before starting. Build fake Greenhouse fixtures/test servers per
§26 before writing the real client against them — don't start with live Greenhouse calls the
way discovery's seed examples were verified; Phase 3's fetch volume is real job content, not
a HEAD/GET probe, and needs the fake-server harness to be safe to iterate on without hammering
a real employer's board.

## 10. Development Continuation Instructions

**Before making any change to this repository:**

1. Read this file (`PROJECT_STATUS.md`) in full.
2. Read `CLAUDE.md` at the repo root (gitignored — present locally, not on GitHub; if it's
   missing, ask Bantamlak for a copy before proceeding, since it carries the authoritative
   project requirements and the mandatory branching/testing/review workflow).
3. PR #3 (Phase 2) is merged to `main` as of this document. Check whether the search-based
   discovery PR (opened by the same session that wrote this update, branch
   `Bantamlak21/phase3-search-discovery-e451cddd`) has since been merged — if it has, this
   document's Section 3 "being committed/pushed" language is stale; update it rather than
   trusting it. Also check whether real `GOOGLE_SEARCH_API_KEY`/`GOOGLE_SEARCH_ENGINE_ID`
   have been provided since — if so, Section 8's `internal/search` known-issue entry needs
   the live-verification follow-up actually performed, not just planned.
4. Run the verification commands in [Section 7](#7-testing-and-verification-status) to
   confirm the state this document describes still matches reality — this document is a
   snapshot, not a live source of truth. If something doesn't match, trust the repository
   and update this document, not the other way around.
5. Follow CLAUDE.md's "Branching" section exactly: one worktree per session, never write in
   the shared checkout (`/home/bantamlak/my-repos/remote-job-aggregator`).

**After completing meaningful work:**

1. Update this document's relevant sections — Section 2 (move newly-completed work out of
   Section 4), Section 3, Section 5 if the schema changed, Section 8, and Section 9 (name the
   new next step).
2. Do not let this document drift from the actual repository state. A stale status document
   is worse than none, since it will be trusted.
3. Follow CLAUDE.md's commit/push/PR ritual for the code changes themselves; update this
   file in the same commit as the work it describes when possible, so the two never fall out
   of sync. (Phase 2's own code and this update landed in separate commits on the same PR
   because of a session interruption — not the ideal pattern, but the PR as a whole still
   ties them together.)
