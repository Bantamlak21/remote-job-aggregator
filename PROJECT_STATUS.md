# Project Status

_Last updated: 2026-09-24._

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

**Phase 1 and Phase 2 are merged to `main`.** The search-based discovery mechanism (PR #4,
originally Google Custom Search) and its follow-up provider swap to Serper (PR #5, Google's
API having become unviable and Brave's free tier requiring a credit card) are both merged.
See Section 2's "Search-based discovery" subsection for that history.

**Phase 3 (ATS ingestion) is implemented**: `aggregator ingest` fetches every active
target's jobs from Greenhouse's public Job Board API and stores them (`internal/ats`,
`internal/ats/greenhouse`, `internal/job.Store`, `internal/ingestion`) — 575 real jobs from
four real boards are in the dev database, served by `aggregator serve` (real Postgres data by
default) to the separate React frontend (`remote-job-aggregator-web`). The public job API
(PR #6) and the search-discovery work (PRs #4/#5/#7/#8) are merged. Filtering, ranking,
scheduling, and notification are not built: every ingested job is `remote_type` /
`employment_type = unknown` with no tags until Phase 4 classifies them. See Section 2's
"Phase 3" subsection for what was built, verified live, and reviewed.

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

**Status: complete, reviewed, split across two PRs.** The original discovery mechanism —
Google Custom Search, two rounds of independent adversarial critic review, round 2 ACCEPT —
is **merged to `main` as PR #4** (commit `86e4506`). A same-session follow-up swapping the
search provider to Serper — its own independent cold critic pass, ACCEPT, no blockers — is
**open, not yet merged, as PR #5** (commit `d6f1512`,
https://github.com/Bantamlak12/remote-job-aggregator/pull/5). No schema migration; one
query-level behavior change to an existing table, shipped in PR #4 (see below).

**Why this exists:** the user explicitly redirected discovery away from hand-curated seed
data — "Instead of seeding companies, or faking it, it's better to work with a real search."
— toward a real web search API. First built against the Google Custom Search JSON API (the
option that was, at the time, both genuinely free and required no credit card, per the
user's own constraint: "Any API as long as it is free and real"), then swapped to Serper
(google.serper.dev) after the user reported the Google API had become unviable for this use
and that Brave's API — the other candidate — requires a credit card even for its free tier.
Serper's free tier (2,500 queries, confirmed against serper.dev's own landing page — "No
credit card required") satisfies the same constraint and is simpler to configure: it wraps
ordinary Google search directly, needing only one credential rather than an API key plus a
separately provisioned "Programmable Search Engine" id.

**⚠ Security incident during this swap, now resolved:** the user's own first draft of a
Serper client (pasted directly, not committed) had a live Serper API key hardcoded in
plaintext. That key was flagged as compromised the moment it was seen and the user was told
to rotate it at serper.dev immediately, independent of any code fix — rotating exposed
credentials is not something a code change can undo. The real implementation below sources
the key from `SearchConfig`/the environment only; grepped to confirm the literal key string
appears nowhere in this repository.

**`internal/search`** (rewritten this session) — wraps the Serper search API.
- `Client.Search(ctx, query) ([]Result, error)`. Config is just `APIKey` — Serper needs no
  second id, unlike Google Custom Search.
- POST with a JSON body (`{"q": ..., "num": 10}`), not a URL query string — the query itself
  never touches the URL, so the credential-in-URL leak class the original Google client had
  to fix (a query-string key leaking through Go's own unredacted `*url.Error` on a network
  failure) cannot occur here structurally, independent of the header-vs-query-string choice.
  The key still travels as the `X-API-KEY` header regardless, matching Serper's own
  documented mechanism.
- Response parsing (`organic[].{title,link,snippet}`) and the error shape
  (`{"message","statusCode"}`) are verified against Serper's own landing page plus two
  independent real-world error reports (a 400 "Missing query parameter", a 403
  "Unauthorized.") found via web research — not guessed. Serper does not appear to expose any
  way to distinguish "bad API key" from "out of free-tier credits" (both produce the same
  generic 403), so unlike the Google client's `ErrQuotaExceeded` (which could recognize a
  specific quota reason code), this client's `ErrUnauthorized` honestly covers both cases
  without claiming to tell them apart.
- Tests: `internal/search/serper_test.go` (11 tests) — result parsing, no-results, the
  documented request shape (POST, JSON body, `q`/`num` fields), the header-not-URL/body key
  placement, a real network-error probe against a nothing-listens-here address confirming the
  key never appears in the resulting error string, the documented 400 and 401/403 error
  shapes, unparseable success and error bodies, context cancellation.
- **Not yet verified against the real google.serper.dev endpoint** — no `SERPER_API_KEY`
  exists in this environment. All verification so far is against fake `httptest` servers,
  and the response/error shapes above are verified against documentation and real-world
  reports, not a live call this session made itself. This is a named, outstanding gap — see
  Section 8.

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
- `CandidatesFromSearch` runs searches sequentially, not concurrently — Serper's free tier is
  a fixed pool of query credits, not a throughput problem worth a worker pool over. One
  company's search failure never stops the batch; every name is attempted and every per-name
  error collected.
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

**`internal/config`** — `SearchConfig` (`SerperAPIKey`, `Configured()`, redacting
`LogValue()`), read from the single `SERPER_API_KEY` env var — simpler than the
originally-built Google version, which needed two env vars validated as a pair. Round-1
review (against the Google version) flagged that the config-level redaction test only called
`.LogValue().String()` directly, never proving redaction survives real `slog` attribute
resolution — fixed with `TestNewLogger_RedactsSearchConfigCredentials` in
`internal/observability/logger_test.go` (mirrors the existing `DatabaseConfig` version, now
updated for the single-field `SearchConfig`), and `cfg.Search` is still logged in
`runSearchDiscover` so `LogValue()` is exercised in real code, not just tests.

**CLI — `aggregator search-discover [company-names-file]`** — loads company names, searches
each, validates+persists any board found via the same `Discoverer.Run` pipeline
`discover` uses (HEAD/GET probing, then persistence) — "how a candidate was found" (seed
file vs. search) is fully decoupled from "what happens once we have one." Requires
`cfg.Search.Configured()`; fails fast, before touching the database, if company names can't
be loaded or no board is found for any of them. Tests:
`TestRunSearchDiscover_RejectsTooManyArgs`, `_RequiresSearchCredentials`,
`_MissingNamesFileFailsBeforeTouchingTheDatabase`.

**`configs/company_names.txt`** (new) — originally shipped with two unverified example names
(Spotify, Airbnb); **updated 2026-09-23, after real `SERPER_API_KEY` credentials arrived**,
to six names (Spotify, Airbnb, GitLab, Notion, Discord, Figma), all live-verified by running
`search-discover` for real against the real Serper API and a real Postgres database — 6/6
found and persisted, 0 failures. See the "Search-discover live-verified" entry below for the
run this session that surfaced why this file mattered (Bantamlak's report that "search only
gets Spotify and Airbnb" — correct behavior given the file's old two-name contents, not a
bug in `search-discover` itself, which was live-confirmed working).

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
real Postgres instance. Those two rounds reviewed the Google-backed implementation.

**The provider swap to Serper (this session) had its own independent cold critic pass:
ACCEPT, no blockers.** The critic read every changed file, walked every documented Serper
response/error shape (success, no-results, 400, 401/403, 500 with a non-JSON body,
malformed JSON on a 200) against `serper.go`'s actual handling and found none mishandled;
confirmed the API key never touches a URL or an unredacted log line and is exercised through
a real `slog.Handler`, not just a direct method call; grepped the whole repo (including git
history) for any leftover Google-specific assumption (`ErrQuotaExceeded`, `GoogleAPIKey`,
`SearchEngineID`, `GOOGLE_SEARCH`) or a live-looking key literal and found none; independently
ran the full build/vet/fmt/test suite, including the DB-backed packages against real
Postgres, and cross-checked every per-package test count against Section 7's table — all
matched exactly; and confirmed README.md/`.env.example`/this document accurately describe
Serper (not stale Google claims) and do not overclaim verification that didn't happen.

**PR #5 has since merged to `main`** (commit `0e93efe`, merge commit `ddccbb0`) — the search
provider swap described above is fully landed, not just reviewed.

---

### Public job API and job-board frontend (this session, complete)

**Status: complete. Two independent adversarial critic passes, run in parallel, both
ACCEPT.** No schema migration.

**Backend critic (Go API): ACCEPT.** Verified the contract match, pagination edge cases,
concurrency safety of `MockRepository`, CORS/preflight behavior, graceful shutdown and
bind-failure handling, and hostile-input handling (unicode, oversized query strings,
path-traversal-looking ids) — all live against a real running server, not just the test
suite. One non-blocking nit: `fixtureJobs`'s doc comment claimed coverage of "every
RemoteType" while no fixture actually used `onsite`. Fixed by correcting the comment (not
by forcing a fixture into an onsite label that didn't fit any of the 12 companies) — zero
ripple into the many places that reference "12 fixture jobs" by count.

**Frontend critic (React UI): ACCEPT.** Verified all 8 frozen rubric items against real
headless-Chrome screenshots (desktop, mobile/360px — genuinely zero horizontal scroll, light
mode via a forced `prefers-color-scheme`, the detail page, and a live-triggered error state)
plus direct code reads (the `rel="noopener noreferrer"` on the Apply link, focus-visible
states, aria-labels, the debounce/clear race's actual fix). Found one real, non-blocking bug:
a hand-edited out-of-range `?page=` produced a real total next to a contradictory "no jobs
match your filters" empty state — not reachable through the UI itself (Pagination hides once
there's only one page), only by editing the URL directly. **Fixed** (commit `92b8fae` in
`remote-job-aggregator-web`): an out-of-range page is now treated as transient — the URL
self-corrects to the last valid page and the skeleton shows instead of the contradictory
combination, verified fixed with a live headless-Chrome screenshot of the exact
previously-broken URL. Also closed a named test-coverage gap: the debounce/clear race was
correct in code but not exercised by the committed suite — added the exact scenario as a
regression test. Frontend suite is now 31 tests (was 29), still 100% passing, `npm run
build` still clean.

**Why this exists:** the user asked to "continue building the frontend with amazing UI/UX
design before moving to phase 3," resolved through two rounds of clarifying questions (recorded
here for continuity) to: (1) scope — a public job-board preview (search/browse/filter jobs)
built now against mock data, not an internal ops dashboard over the real
companies/target_companies data; (2) location — a brand-new, separate sibling git repository,
not a subdirectory of this repo; (3) stack — a React SPA talking to a new Go JSON API, not
server-rendered Go templates.

**`internal/job`** (new package) — the normalized `Job` domain type, `RemoteType`/
`EmploymentType` enums (each with a `.Valid()` method), the `Filter`/`ListResult` types, and
the `Repository` interface `internal/api` depends on. `MockRepository` is the only
implementation today: 12 hand-written, illustrative fixture jobs (real, verifiable company
career-page domains; synthetic job content, clearly documented as such in the package
comment) spanning every `RemoteType`/`EmploymentType` value. Deterministic: no `time.Now()`,
fixed `PostedAt` dates, safe for concurrent reads (immutable slice after construction, no
lock needed). `List` filters (query/remote_type/employment_type/company/tag), sorts
newest-first (ties broken by ID), and paginates; `Get` returns `ErrNotFound` for an unknown
id. Tests: `internal/job/mock_test.go` — every filter independently and in combination,
pagination boundaries (exact page, past the last page), context cancellation, both enums'
`.Valid()`.

**`internal/api`** (new package) — the HTTP layer. `NewHandler` wires two routes
(`GET /api/v1/jobs`, `GET /api/v1/jobs/{id}`, using Go 1.22+ stdlib `http.ServeMux` method+
pattern routing — no third-party router, per "vanilla by default") through CORS and
request-logging middleware, over `job.Repository` — never a concrete type. `parseFilter`
validates every query parameter at the boundary (invalid `remote_type`/`employment_type`,
non-numeric or out-of-range `page`/`page_size` are all `400`s with a specific message, never
silently ignored or clamped). DTOs (`jobSummaryDTO`/`jobDetailDTO`) mirror `docs/api.md`
exactly, including `company_logo_url` serializing as JSON `null` (via a `*string`) rather
than an empty string. CORS allows exactly one configured origin, never a wildcard, and an
`OPTIONS` preflight is answered directly without ever reaching the mux/repository. Tests:
`internal/api/handlers_test.go` — every documented success/error path, the null-logo-URL
serialization, CORS headers present, preflight never touches the repository (a
panic-if-called fake proves it), RFC3339 `posted_at` format.

**`internal/config`** — added `APIConfig` (`Addr`, `CORSAllowedOrigin`), env vars `API_ADDR`
(default `:8080`, validated via `net.SplitHostPort`) and `CORS_ALLOWED_ORIGIN` (default
`http://localhost:5173`, must be non-blank). Tests: defaults, overrides, both rejection paths.

**CLI — `aggregator serve`** — starts `api.NewServer` backed by `job.NewMockRepository()`,
integrated with the same signal-handling/graceful-shutdown pattern `runApp` already
established. A real bug was caught by the test written for this (see below), fixed, and left
documented in a code comment rather than papered over.

**Bug found by its own test, fixed:** the first version of
`TestRunServe_StartsRespondsAndShutsDownCleanly` made a plain `http.Get` in a polling helper
and closed the response body without draining it first. That left the connection in a state
Go's `http.Server.Shutdown` doesn't consider "idle" promptly, so the test's shutdown wait hung
past its 5-second deadline. Fixed the test (drain the body before closing) and, since the
underlying behavior is real and worth knowing operationally — not just a test artifact —
added a comment on `runServe`'s `Shutdown` call explaining that a client which doesn't drain
response bodies can make a graceful shutdown take up to the full `SHUTDOWN_TIMEOUT` instead of
returning instantly. Bounded, not a hang, but worth knowing if a deploy ever seems to wait the
full timeout on this command.

**Manual live verification performed** (beyond the automated test suite): built the real
binary, ran `aggregator serve` against the real running Postgres container's connection
string (unused by `serve` itself, but still required by `config.Load()`), and `curl`'d every
documented path — list with pagination, detail fetch, invalid `remote_type` (400), unknown id
(404), OPTIONS preflight (204 with the right CORS header) — confirming the real server's
output byte-for-byte matches what the automated tests assert.

**`internal/discovery`, `internal/company`, `internal/search`, `internal/config`'s existing
fields: untouched.** This is additive — no existing behavior changed except the new
`APIConfig` fields being added to `Config`.

---

### remote-job-aggregator-web (new, separate repository)

**Status: complete — implemented, tested, live-verified, independently critic-reviewed
(ACCEPT), one bug found by that review fixed and re-verified (see the "Public job API and
job-board frontend" subsection above for the critic findings and the fix).** Lives entirely
outside this repository at `/home/bantamlak/my-repos/remote-job-aggregator-web`
— its own git history, `git init`'d fresh this session, all work on a local branch `init`
(not `main`), **no remote configured and nothing pushed anywhere** — that decision (GitHub or
not, and where) is explicitly deferred to Bantamlak, not assumed.

**Stack:** React 19 + Vite 7 + TypeScript + Tailwind CSS v4 + React Router 7. No heavier
component runtime (no shadcn/ui, no Next.js) — every card/badge/filter control is a small,
hand-built Tailwind component, a deliberate choice documented in that repo's own README, not
an oversight.

**Built:** a list view (`/`) — debounced search, filters for remote type/employment
type/tag, a responsive job-card grid, numbered pagination with a visible total count, and
designed loading/empty/error states (not spinner-only, not a blank screen) — and a detail
view (`/jobs/:id`) with the full description, an "Apply for this role" CTA (opens
`application_url` in a new tab), and a back link that restores prior filter/search/page
state. Light/dark/system theme with a manual toggle, persisted, with an anti-flash inline
script. Filter/search/page state lives in the URL query string (shareable, survives a
refresh).

**A real bug was found and fixed during that session's own test-writing** (not by this
session, but recorded here since it's directly relevant to the frozen "clear filters must
actually clear" acceptance criterion): the original debounce design let a stale, already-in-
flight debounced search value race a `clearFilters` call and silently re-write the just-
cleared search term back into the URL a moment later. Fixed by reading the search term
directly from the URL and using a cancelable manual timer `clearFilters` cancels
synchronously; caught by that repo's own `useJobFilters.test.tsx`.

**Tested:** Vitest + React Testing Library, 29 tests across 6 files, all passing (API client
query construction, job card field rendering, filter-state URL round-trip including the bug
above, empty/error state rendering, a `JobListPage` integration suite mocking fetch end to
end). `npm run build` (`tsc -b && vite build`) compiles with zero TypeScript errors.

**Manually verified by this session, independent of the builder's own report** — real
screenshots via headless Chrome (`/usr/bin/google-chrome --headless=new`) against the real
running Go backend, not just code-reading:
- Desktop (1440px): dark-theme job-card grid renders real fixture data — companies, titles,
  colored remote/employment-type badges, tag pills, relative posted dates ("2 days ago"),
  search bar, three filter dropdowns, "12 jobs found" count. Genuinely polished — restrained
  indigo/dark palette, consistent spacing, readable at a glance.
- Mobile (360px): single-column layout, no horizontal scroll, filters wrap into a compact
  stack, long titles truncate cleanly (e.g. "Backend Engineer, Payme…") rather than
  overflowing or wrapping awkwardly.
- Detail page: full description text pulled live from the real API, styled "Apply for this
  role" CTA, working "Back to results" link.
- Light mode was independently re-verified by the frontend critic pass (forced
  `prefers-color-scheme` via a Chrome `--blink-settings` flag, confirmed against
  `ThemeProvider.tsx` and the anti-flash inline script's matching logic): fully readable,
  correct contrast, no leftover dark-only elements. The gap noted earlier in this session's
  own manual pass is closed.

**Known, documented limitation (the builder's own, not hidden):** the API has no "list all
tags" endpoint, so the tag-filter dropdown's options are sampled from one `page_size=100`
request's distinct tags. Correct today (12 jobs, one page), will under-count rare tags once
real volume ships post-Phase-3. Flagged in that repo's own code comment and README.

---

### Phase 3: ATS integration & ingestion (Greenhouse)

**Status: implemented, live-verified against real Greenhouse boards and the real dev
database; independent adversarial critic review recorded at the end of this subsection.**
No schema migration (the Phase 1 `jobs` schema was already sufficient).

**A schema/contract mismatch found first, fixed before building on it.** The `jobs` table's
CHECK constraints allow `remote_type IN ('remote','hybrid','onsite','unknown')` and
`employment_type IN ('full_time','part_time','contract','internship','unknown')`, but the
mock job API built earlier this session used `fully_remote` and had no `unknown` — every real
insert would have been rejected by Postgres. `internal/job`'s enums now match the schema
exactly; `docs/api.md`, the handlers' validation messages, and the separate frontend repo's
types/badges/filters were updated to match (an `unknown` value renders no badge, since a
pill on every card of a real board would be noise).

**Verified against the real API, not assumed.** Greenhouse's public Job Board API
(`GET boards-api.greenhouse.io/v1/boards/{token}/jobs?content=true`, no key) was curl'd for
real boards (gitlab, figma) before any code was written: 404 for an unknown board
(`{"status":404,"error":"Job not found"}`), and — a real finding — the `content` field is
HTML-entity-encoded at the *outer* layer (the JSON string is literally `&lt;div&gt;...`, not
`<div>...`). A first `HTMLToText` that tokenized directly returned literal tag text as
"plain text"; caught by a fixture test built from the real response, fixed by unescaping once
before tokenizing.

**`internal/ats`** — the provider-agnostic `ats.Job` shape (`ExternalID`, `Title`, `URL`,
`LocationRaw`, plain-text `Description`, `PublishedAt`), the shared `ats.ErrBoardNotFound`
(shared, not per-provider, so ingestion can react to it without importing any provider), and
`HTMLToText` (real HTML tokenizing via `golang.org/x/net/html`, the one new dependency — chosen
over a regex tag-stripper, which breaks on nested tags/attributes containing `>`).
**`internal/ats/greenhouse`** — `Client.ListJobs` over the shared pooled `httpclient.Client`
(retries/backoff/size cap inherited), always `content=true`.

**`internal/job`** — `Record` (full persistence shape) and `Store`: `UpsertFromATS` is one
atomic `INSERT ... ON CONFLICT (source, source_job_id) DO UPDATE` (a CTE captures the
previous `content_hash`; `last_seen_at` advances on every sighting, `last_changed_at` and the
content columns only when the hash differs; a re-appearing job reopens), with the same
`WHERE` guard against cross-target reassignment `TargetStore.Upsert` got in the search-discovery
work (`ErrJobTargetMismatch`). `MarkMissingAsRemoved` closes a target's open jobs absent from
the latest fetch in one `UPDATE ... <> ALL($2::text[])`. `List`/`Get` (the public
`Repository`) serve only `open` jobs. **Two real bugs were caught by this package's own
Postgres tests**, not by review: an unaliased `COALESCE` referenced as `posted_at` in
`ORDER BY`, and an ambiguous `description` (both `jobs` and `companies` have one) — plus a
subtle one worth remembering: pgx encodes a `nil` Go slice as SQL `NULL`, and
`x <> ALL(NULL)` is `NULL` for every row, so `MarkMissingAsRemoved(ctx, id, nil)` silently
removed nothing instead of everything; fixed in the store by normalizing nil to an empty slice.

**`internal/ingestion`** — bounded worker pool mirroring `internal/discovery.Run`, one
target per worker. A board-not-found deactivates the target (permanent signal); any other
error leaves it active (may be transient); and a failed fetch **never** reaches
`MarkMissingAsRemoved` (an empty result from a failure would wrongly close every real job) —
each pinned by a test. Unregistered providers are per-target errors, never silent skips.
`TargetStore` gained `ListActive`, `MarkIngestionSucceeded` (finally writing the
`last_successful_ingestion_at` column Phase 1 created), and `SetActive`.

**CLI/config** — `aggregator ingest`; `INGESTION_WORKERS` (default 5, 1-100);
`JOB_REPOSITORY=postgres|mock` (default `postgres`; `mock` keeps the 12 fixture jobs for
frontend development with no database). `serve` now connects to Postgres by default.

**Live verification (real network, real Postgres):** `ingest` against the dev database's six
discovered targets ingested **575 real jobs** (Discord 49, Figma 159, Airbnb 160, GitLab 207);
a second run reported 575 *unchanged*, 0 inserted (idempotent, content-hash change detection
working on real data); planting a phantom open job under a target and re-ingesting reported
`removed: 1` (removal detection working live; phantom then deleted). Spotify (Lever) and
Notion (Ashby) are reported `no ATS client registered` every run — expected until Phase 7.
The real frontend was screenshotted (headless Chrome) rendering the real jobs.

**Review round 1 (independent cold critic): REJECT, 3 blockers + 4 should-fix — all fixed.**
The critic measured live boards and probed against real Postgres; nothing below was theoretical.
- *B1: the shared 5 MiB HTTP response cap permanently failed large boards.* Measured live:
  databricks (883 jobs) is 9.7 MB decoded and stripe (695) 5.4 MB; gitlab/airbnb/figma fit.
  Fixed with an ingestion-specific client (`INGESTION_MAX_RESPONSE_SIZE`, default 32 MiB;
  `INGESTION_HTTP_TIMEOUT`, default 30s) and stream decoding (no `io.ReadAll`). Re-verified
  live: databricks fails at the old 5 MiB cap ("response body exceeds the configured maximum
  size") and fetches 883/883 jobs with descriptions in 1.4s at 32 MiB; stripe 695/695.
- *B2: a 200 with no `jobs` key silently meant "zero jobs", which closes every job on the
  board.* Now an error (`Jobs` is a pointer; absent != empty).
- *B3: the board token was interpolated into the URL path unescaped* (`acme?x=1#` swallowed
  `/jobs`; `../../evil`). Now validated against `^[A-Za-z0-9_-]+$` (`ats.ErrInvalidBoardToken`,
  no request made) and `url.PathEscape`d.
- *S1: one bad job (NUL byte, invalid UTF-8, blank title/URL) aborted the whole board on every
  run.* Fields are sanitized (`ats.CleanText`); jobs missing id/title/url are skipped; a job
  Postgres refuses (SQLSTATE class 22/23, via `job.IsRejectedRecord`) is skipped and counted in
  `Result.Skipped`, while infrastructure errors still abort. A skipped job still counts as
  "seen", so it is never closed out as vanished.
- *S2: `jobs.canonical_url` is a global unique index; setting it from the job URL let one
  duplicate URL freeze a board.* Ingestion no longer sets it (identity is
  `(source, source_job_id)`).
- *S3: a 404 deactivated the target but left its jobs open with dead links forever,
  contradicting the API docs.* A gone board now also closes its jobs; both are recoverable
  (rediscovery reactivates the target; a reappearing job reopens on the next ingest).
- *S4: `HTMLToText` corrupted real text* (`Note : Java TM`, script/style bodies leaking,
  `<br/>` ignored, nested-list blank lines, and an unconditional pre-unescape that turned
  legitimate `&lt;b&gt;` text into a swallowed fake tag). Rewritten; the pre-unescape now only
  applies when the input has no literal `<` but does contain `&lt;` (Greenhouse's outer-encoded
  shape). The critic's "idempotent" comment claim was also false and is gone.
- Nits fixed: `ILIKE` metacharacters (`%`, `_`) now match literally; a page past the last one
  still reports the real total (the frontend's out-of-range self-correction depends on it); a
  reopened job now counts as changed and bumps `last_changed_at`; titles are trimmed;
  `PublishedAt` comes only from `first_published` (`updated_at` moves on every edit and the
  store freezes `published_at`); `docs/api.md` no longer claims `q` matches tags for real jobs.
  Each has a regression test. After the fixes a live re-ingest reported the expected one-time
  change (574 changed: cleared canonical URLs and cleaner description text; plus genuine
  board churn: GitLab +1, Discord -1), and an immediate second run was fully idempotent
  (575 unchanged).

**Review round 2 (fresh cold critic): ACCEPT, no blockers.** It independently re-verified every
round-1 claim against real data: 17 hostile board tokens (none reached the network), 20 bad-job
cases against real Postgres (NUL/invalid-UTF-8 -> 22021, blank title/URL -> 23514, all skippable),
the databricks board (883 jobs, 9.69 MB) failing at 5 MiB and fetching at 32 MiB, and
`HTMLToText` over all 1,737 live jobs from stripe/databricks/figma plus the ~576 stored
descriptions: zero leftover entities, tag fragments, control characters, or marker leakage;
deep-nesting/5 MB-text-node/2M-`<br>` inputs all finished in under 400 ms. Its findings, all
fixed with regression tests: (1) the B1 fix had no regression test — added a >5 MiB board test
(fails at the shared cap, succeeds at the ingestion cap) and a `cmd/aggregator` test pinning
`newIngestionHTTPClient`; (2) a huge `?page=` overflowed `(page-1)*pageSize` into a negative
`OFFSET`, a 500 from a public endpoint — now a normal past-the-end page with the real total
(and a test that a filtered past-the-end total respects the filters); (3) foreign-key
violations (23503) were classed as skippable per-job rejections, which would report a
"successful" run that stored nothing if a target vanished mid-run — now they abort; (4) a job
with a missing id became the colliding identity `"0"` — now empty, so ingestion skips it; (5)
an inaccurate "memory bounded by one job" comment corrected (the decoded board is held whole,
~13x the JSON size transiently; worker count x board size bounds memory). Left as documented
behavior, not bugs: `<pre>` whitespace collapses, input with `&lt;` but no `<` is always treated
as outer-encoded, and one wrong-typed JSON field fails a whole board's decode (Greenhouse ids
are reliably ints). The optional single-404 debounce is unbuilt (Section 8).

### Ethiopian priority companies (between Phase 3 and Phase 4; branch `Bantamlak21/ethiopian-priority-e451cddd`)

**Request:** the jobs of 25 Ethiopian tech companies (Bantamlak's list, `configs/ethiopian_companies.json`)
must be on the board, pinned first, badged, with a filter. **Design and measured results:**
[docs/priority-companies.md](docs/priority-companies.md). Summary:

- **Schema/API/UI:** migration `000002` adds `companies.is_priority`; `job.Store.List` orders
  `is_priority DESC, posted_at DESC, id DESC` (pin holds across pages; real-Postgres tests);
  `is_priority` on list and detail; `?priority=true` filter (`false` = no filter, anything else
  400); frontend (`remote-job-aggregator-web`) badge, violet card outline and "Ethiopian
  companies only" toggle, 43 tests.
- **Sources** (none of the 25 is on Greenhouse/Lever/Ashby): `internal/ats/feed` (RSS),
  `internal/ats/careers` (a company's own careers page), `internal/ats/jobsearch` (Serper search
  over LinkedIn and Ethiojobs). All three fit the existing target/ingestion model as new
  `ats_provider` values. Supporting: `internal/robots` (RFC 9309, fail-closed, redirects gated),
  `internal/ats/page` (robots-gated, size-bounded fetch/parse), `httpclient.WithRedirectCheck` /
  `WithoutRetries`. New commands: `seed-priority`, `priority-report`, `ingest --providers=`.
- **Trust rules found by running against the real sites (2026-09-24):** company sites and feeds
  are stale (Kifiya's careers site still lists Feb 2025 postings; EthSwitch/ZalaTech feeds end
  in 2024/2023; Zare's five postings expired in July), so dated items older than 120 days or
  past `validThrough` are dropped; 12 of 13 sampled Ethiojobs postings were closed, so only
  `status: active` with a future expiry is accepted; search returns look-alike companies (Chapa
  De Indian Health, Chaka Gebeya) and a same-named company abroad (DreamTech, Noida), so a
  LinkedIn result needs an exact company match in its URL slug plus an Ethiopia signal
  (`hires_outside_ethiopia` is set only for Gebeya). LinkedIn pages are never fetched.
- **Search source is opt-in and metered:** two Serper queries per company (50 per run), one
  call = one wire request, capped by `SEARCH_MAX_QUERIES_PER_RUN` (default 60), never part of a
  plain `ingest`. Search is a sample, so its jobs close only after 21 unseen days
  (`ingestion.PartialClient`), except jobs the source reports as ended, which close at once
  (`ats.Job.Closed` + `job.Store.CloseBySourceID`).
- **Measured coverage:** 4 of 25 companies have an open job (8 jobs: EthSwitch 4, Addis Software
  2, Kifiya 1, Zare 1); 11 of the 25 have no findable careers page or feed and search found
  nothing current for them. Report: `aggregator priority-report`.
- **Review:** two independent adversarial critic rounds. Round 1 REJECT (redirects bypassed
  robots.txt, Serper retries multiplied wire requests past the budget, robots parser failed open
  on BOM/CR/percent-encoding/prefix-agent, ended jobs lingered, silent truncation, dedupe merged
  different-country openings); all fixed with tests through the real `httpclient`. Round 2
  ACCEPT with should-fixes (feed date forms, query-string job identity, non-job careers links,
  Ethiojobs fetch spacing, duplicate copies, non-ASCII titles), all fixed; 28 mutations of the
  load-bearing logic were all caught by the tests. Critic files: `/tmp/ethiopian-priority/critique/`.
- **Known limits (also in the doc):** a 404 on a careers page or feed deactivates its target
  until `seed-priority` is re-run; a careers page with zero job links is an error (so a
  company's last removed posting lingers until someone looks); search returns 10 results per
  query; `nameKey` drops `Ethiopia`/`Co`/`PLC` suffixes.

## 3. Work In Progress

PRs #3–#9 (Phase 2, search discovery, the job API, the company-names fix, the deferred-
feature note, Phase 3) are merged. The **Ethiopian priority companies** work (Section 2) is
code-complete, reviewed twice, and open as **PR #10**
(https://github.com/Bantamlak12/remote-job-aggregator/pull/10), not yet merged as of this
writing; its frontend changes (badge, filter, tests) are committed locally in
`remote-job-aggregator-web` (branch `init`, no remote). The Phase 3 notes below are historical:

1. The Phase 3 backend change (branch `Bantamlak21/phase3-ats-ingestion-e451cddd`) is open as
   **PR #9** (https://github.com/Bantamlak12/remote-job-aggregator/pull/9), not yet merged as
   of this writing.
2. `remote-job-aggregator-web` (a separate repo, branch `init`, no remote — whether/where it
   gets one is Bantamlak's call) has the enum-alignment change (types/badges/filters for
   `remote`/`unknown`) committed locally (`c4ede8f`) but, like the rest of that repo, pushed
   nowhere.

Whoever picks this up next: check whether both have moved, rather than trusting this paragraph
once time has passed.

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
list. Phase 3 (ATS integration & ingestion, Greenhouse first) is also complete — see
Section 2 — and removed from this list.

### Phase 4 — Geographic eligibility & relevance filtering
- **Objective:** deterministic classification of remote-eligibility and role relevance.
- **Tasks:** `filtering` package — geographic classifier (ELIGIBLE/INELIGIBLE/UNCERTAIN
  with confidence, reasons, evidence, detected locations, restrictions) and relevance
  filtering (configurable positive/negative signals, not hard-coded to one job title). New
  migration adding `job_eligibility` (deferred from Phase 1 deliberately — see Section 5).
- **Expected files/modules:** `internal/filtering/`, `migrations/00000X_job_eligibility.*.sql`.
- **Dependencies:** Phase 3 (done: 575 real ingested jobs to classify, `location_raw` populated).
- **Verification:** unit tests are the primary tool (deterministic logic), plus an eval
  suite per CLAUDE.md's rule that classification quality needs more than deterministic
  tests alone.

### Phase 5 — Ranking & upsert/change detection
- **Objective:** deterministic, explainable scoring, wired into the (already built)
  ingestion pipeline. The upsert-by-`(source, source_job_id)` identity, `content_hash` change
  detection, and open/removed lifecycle originally listed here shipped in Phase 3.
- **Tasks:** `ranking` package (configurable scoring, explainable output); ingestion (or a
  step after it) computing/storing scores for new and changed jobs.
- **Expected files/modules:** `internal/ranking/`; small changes to `internal/ingestion/`.
- **Dependencies:** Phase 4 (jobs need eligibility/relevance data to rank against).
- **Verification:** unit tests for scoring, plus an eval suite for ranking quality.

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
  (`jobs_fetched_total`, `provider_errors_total`, etc.) (no numbered CLAUDE.md section covers this; the earlier reference to "§23" was fabricated).
- **Dependencies:** Phase 3's `ats` abstraction must already support a second/third
  provider without rework.
- **Verification:** same fake-server pattern as Greenhouse.

### Deferred — automatic company discovery (explicitly, not a current phase)

**Not scheduled, deliberately deferred by Bantamlak until after Phases 3–7 ship
(2026-09-23).** `search-discover` only *resolves* a company name you already give it (via
`configs/company_names.txt`) to a real ATS board URL — it does not discover which companies
exist. Bantamlak initially expected the latter and was told plainly it isn't built; his
decision was to implement it later, after the rest of the roadmap, not now.

What this would actually require, if picked up later — not designed yet, just named so the
scope isn't lost: a query strategy that surfaces real companies rather than one already-named
company (e.g. searching for something like `site:boards.greenhouse.io "remote"` and parsing
company names out of result URLs/titles, or a fundamentally different query shape); dedup
against companies already in `companies`/`target_companies`; and a rate-limit-aware design
given Serper's free tier is a fixed pool of query credits, not a per-day renewing budget —
an open-ended discovery loop could burn that pool fast without a hard cap. Needs its own
Confusion-Protocol-style scoping conversation before building, not a quick bolt-on.

## 5. Current Database State

**Migrations:** two — `000001_init_schema` and `000002_company_priority` (adds
`companies.is_priority BOOLEAN NOT NULL DEFAULT false`), each with up + down, applied and clean
(`schema_migrations`: version 2, dirty=false). `migrate-down` (one step) now undoes only 000002.
Phase 2 added no migration; it only added Go code (`internal/company`) over the tables Phase 1
already created.

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
afterward). `jobs` is now written by `internal/job.Store` via `aggregator ingest` (Phase 3) — 575 real jobs in the dev database as of 2026-09-24.

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
    ├─ internal/search          (Serper search API client)
    ├─ internal/company         (Store/TargetStore over companies/target_companies)
    ├─ internal/discovery       (seed OR search → candidates → probe → persist)
    ├─ internal/ats             (ats.Job shape, HTMLToText, ats/greenhouse client)
    ├─ internal/job             (Job/Record types, Repository, Store (Postgres), MockRepository)
    ├─ internal/ingestion       (bounded worker pool: targets -> ATS fetch -> job.Store)
    ├─ internal/api             (public read-only job API — serve command)
    └─ configs/                 (seed_companies.json, company_names.txt)

remote-job-aggregator-web (separate repo, not in this tree)
    └─ React SPA — job-board preview UI, talks to internal/api over HTTP
```

One deployable binary. `cmd/aggregator/main.go` is the only place that wires concrete
implementations together. Nothing outside `internal/database` touches `pgxpool` directly;
nothing outside `internal/config` reads `os.Getenv`; nothing outside `internal/search` talks
to the Serper API. `internal/discovery` depends on `internal/company`,
`internal/httpclient`, and (for the search path) `internal/search` through small interfaces
it defines itself (`CompanyUpserter`, `TargetUpserter`, `SearchClient`) rather than their
concrete types — the consumer-defines-the-interface Go convention, chosen so discovery's own
orchestration logic (HEAD/GET fallback, concurrency bound, error propagation, board-URL
recognition) is unit-testable with fakes instead of always needing a database or a live
Serper API key. Two independent ways to produce a `Candidate` (`LoadSeedCandidates` from a
JSON file, `CandidatesFromSearch` from a search) converge on the same `Discoverer.Run`
pipeline — "how a candidate was found" is fully decoupled from "what happens once we have
one."

**Planned, not implemented:** `internal/filtering`,
`internal/ranking`, `internal/notification`, `internal/scheduler` — see Section 4. These
packages do not exist on disk. Do not assume any of their functionality when reasoning about
what the system currently does.

**Concurrency implemented today:** `pgxpool`'s internal connection management (bounded by
config); the CLI's signal-handling goroutines (`installSignalHandling`, `watchGracefulStop`,
`closeWithTimeout`); `internal/discovery`'s bounded worker pool
(`DISCOVERY_WORKERS`, independent of the database pool and the HTTP client's own connection
pool); `internal/ingestion`'s bounded worker pool (`INGESTION_WORKERS`, one ATS board per
worker, likewise independent).

## 7. Testing and Verification Status

**Run and passing as of this document (2026-09-24, Phase 3 branch, before commit):**

```bash
gofmt -l .                             # clean
go build ./...                         # clean
go vet ./...                           # clean
TEST_DATABASE_URL=postgres://aggregator:aggregator@localhost:5432/aggregator_test \
  go test -race -p 1 -count=1 ./...    # all 13 packages ok, real Postgres, race detector —
                                        #   top-level test functions: 12 internal/ats,
                                        #   11 internal/ats/greenhouse, 16 internal/ingestion,
                                        #   33 internal/job, 36 internal/company,
                                        #   35 internal/config, 24 cmd/aggregator, 11 internal/api
                                        #   (plus unchanged: database, httpclient, discovery,
                                        #   search, observability)
```

`internal/ats`, `internal/ats/greenhouse`, and `internal/ingestion` are new in Phase 3;
`internal/job` gained `Store` (tested against real Postgres); `internal/company`,
`internal/config`, and `cmd/aggregator` gained tests for the new methods/settings/command. All
verified passing individually and as part of the full-suite run above; zero regressions.

**Frontend (`remote-job-aggregator-web`, separate repo):** Vitest + React Testing Library,
32 tests across 6 files, all passing; `npm run build` (`tsc -b && vite build`) compiles with
zero TypeScript errors. Run from that repo's own directory, not this one.

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

**Live verification performed this session for the job API + frontend:** the real
`aggregator` binary run as `serve` against the real Postgres container's connection string
(required by config, unused by the command itself), `curl`'d for every documented path
(list, pagination, detail, 400, 404, OPTIONS preflight) — output matched the automated tests
exactly. The frontend's dev server was run against that real backend and captured with
actual headless-Chrome screenshots (`/usr/bin/google-chrome --headless=new`) at desktop
(1440px) and mobile (360px) widths, plus the job detail route — real rendered data, not a
code-reading guess. Light mode specifically was not re-verified visually this session (see
Section 8).

**Missing tests:** none within Phase 1, Phase 2, or the search-discovery extension's own
scope — every behavior change in this session shipped with a regression test, per CLAUDE.md.
`internal/search/serper.go` has not been tested against the real, live Serper API (see
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
- **RESOLVED 2026-09-23**: `internal/search` has now been run against the real
  `google.serper.dev` endpoint — Bantamlak provided a real `SERPER_API_KEY`. Ran
  `search-discover` end to end (real Postgres, real network) twice: once against 4 new names
  (GitLab, Notion, Discord, Figma) and once against the updated 6-name default
  `configs/company_names.txt` (adds those 4 to Spotify/Airbnb) — 6/6 found and persisted,
  0 failures both times. Confirmed live: header-based auth is accepted by the real endpoint,
  the `organic[].{title,link,snippet}` response shape matches `apiResponse`'s assumptions
  exactly. The `{"message","statusCode"}` error shape (400/401/403) was *not* re-confirmed
  this run — every one of the 6 names succeeded, so no error path was actually exercised
  live; that specific sub-claim is still resting on the research-backed assumption from when
  `serper.go` was written, not a fresh live observation. This entry prompted by Bantamlak
  reporting "search only gets Spotify and Airbnb" — the actual cause was
  `configs/company_names.txt` still only containing those two names, not a defect in
  `search-discover` itself (which this live run confirms works correctly).
- **A hardcoded, live Serper API key was found in a draft file during this session and
  flagged for immediate rotation** (not committed to git, and confirmed absent from this
  repository by direct grep before this document was written) — mentioned here so the
  rotation isn't forgotten if it hasn't happened yet. See Section 2's "Search-based discovery"
  subsection.
- **The job API's data is entirely mock/illustrative.** `internal/job.MockRepository`'s 12
  fixture jobs are not real postings — real `application_url` domains, synthetic titles and
  descriptions. This is by design (Phase 3 hasn't shipped real ingestion) and documented in
  the package comment, `docs/api.md`, and this document, but worth restating so nobody mistakes
  the job-board frontend's contents for real listings before Phase 3 lands.
- **The tag-filter dropdown's vocabulary is sampled, not authoritative.** No endpoint lists
  every tag that exists; the frontend samples one `page_size=100` request's distinct tags.
  Correct today (12 jobs fit one page); will under-count rare tags once real job volume
  ships. Flagged in `remote-job-aggregator-web`'s own code and README, not hidden.
- **`remote-job-aggregator-web` has no remote configured and nothing pushed anywhere.**
  Whether it gets a GitHub remote, and under which account/org, is explicitly Bantamlak's
  call, not assumed by either builder session. All work (13 commits on `init`) is safe
  locally in the meantime.
- **(Superseded: PR #6 merged.)** The Phase 3 known risks below replace this entry.
- **A legitimately-empty-looking board wipes its jobs.** A 200 with `"jobs": []` (key present,
  empty list) is treated as "the board has zero jobs" and closes every open job for that target.
  That is correct for a genuinely empty board, but a Greenhouse hiccup that returned an empty
  list would do the same. Not guarded (no evidence it happens; the recovery is automatic,
  since jobs reopen on the next good run) — a "refuse to remove more than N% of a board in one
  run" guard is the obvious hardening if it ever bites.
- **A single 404 permanently deactivates the target and closes its jobs.** Bounded and
  recoverable (rediscovery reactivates; the next ingest reopens jobs), but there is no
  "N consecutive 404s" debounce.
- **`(xmax = 0)` insert detection in `job.Store.UpsertFromATS` relies on documented-by-usage
  Postgres behavior, not a guaranteed contract.** Under a concurrent first-insert race the
  loser can over-report `Changed`; affects run counters only, never stored data. The
  concurrent-convergence test passes today; re-check it on a Postgres major upgrade.
- **Only Greenhouse is ingested.** Lever and Ashby targets found by `search-discover` are
  reported `no ATS client registered` every run until Phase 7 (documented in the README).
- **Real jobs are unclassified.** Every ingested job is `remote_type`/`employment_type =
  unknown` with no tags and no logo, so the job board's type/tag filters match nothing useful
  until Phase 4. The API and frontend handle `unknown` honestly (no badge, no guessing).
- **`internal/ingestion`'s worker pool uses `wg.Add(1)`/`go func` rather than `WaitGroup.Go`**
  (a gopls modernization hint), deliberately matching `internal/discovery.Run` line for line;
  modernize both together if ever.

## 9. Exact Next Step

**Before Phase 4:** run `aggregator migrate-up`, `seed-priority`, and (with `SERPER_API_KEY`)
`ingest --providers=search` on the target database, and restart `aggregator serve`; schedule
`ingest` daily and the search source weekly (see docs/priority-companies.md). Then:

**Start Phase 4: geographic eligibility & relevance filtering** (`internal/filtering`, plus a
`job_eligibility` migration). Phase 3 deliberately ingests every job as `remote_type =
unknown` / `employment_type = unknown` with empty `tags`, because Greenhouse's public API
has none of those fields; classifying them from the raw `location_raw` string, the title, and
the description is exactly Phase 4's job, and it is where this project's core value
(correct Ethiopia-eligibility) actually lives. Until it lands, the job board lists real jobs
but cannot filter by remote type, employment type, or tag (those filters match nothing or
everything, honestly, rather than guessing).

Prerequisites already in place: 575 real jobs in the dev database from four Greenhouse
boards, `jobs.location_raw` populated, `jobs.remote_type`/`employment_type` constrained to
values including `unknown`, and an API/frontend that already render an `unknown`
classification gracefully (no badge). Design it deterministically (rules and lookup tables,
per the project's LLM-usage rule) with an eval suite for classification quality, using the
real ingested `location_raw` values as the fixture corpus — many are things like
"Remote, Canada; Remote, United States" or "San Francisco, CA • New York, NY" that a naive
"contains 'remote'" rule would misclassify.

Smaller, independent items:
- **Phase 7 (Lever/Ashby) is now blocking real coverage**: `search-discover` found 6 boards,
  but `ingest` can only fetch the 4 Greenhouse ones; Spotify (Lever) and Notion (Ashby) are
  reported as `no ATS client registered` every run. The `ats`/`ingestion` abstraction was
  built for this (a new provider is a new client in the `clients` map in `runIngest`), so
  pulling Phase 7's provider work forward is cheap if Bantamlak wants those companies' jobs.
- The frontend (`remote-job-aggregator-web`) should be revisited after Phase 4 (its filters
  and tag dropdown are only meaningful once jobs are classified), per the standing request to
  update it each phase.

## 10. Development Continuation Instructions

**Before making any change to this repository:**

1. Read this file (`PROJECT_STATUS.md`) in full.
2. Read `CLAUDE.md` at the repo root (gitignored — present locally, not on GitHub; if it's
   missing, ask Bantamlak for a copy before proceeding, since it carries the authoritative
   project requirements and the mandatory branching/testing/review workflow).
3. PR #3, #4, and #5 are all merged to `main` as of this document. Check whether **PR #6**
   (https://github.com/Bantamlak12/remote-job-aggregator/pull/6, the job API) has since
   merged; if so, this document's Section 2/3 "open, not yet merged" framing is stale, update
   it rather than trusting it. Also check: whether a real `SERPER_API_KEY` has been provided
   (Section 8's `internal/search` entry needs its live-verification follow-up actually
   performed if so), and whether
   `/home/bantamlak/my-repos/remote-job-aggregator-web` has since gotten a remote/been pushed
   anywhere (Section 8 notes it had neither as of this writing).
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
