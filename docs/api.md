# Public Job API (v1)

JSON API backing the job-board frontend. The job endpoints are public and read-only: this is
public data by design (job listings are meant to be found). Optional sign-in and saved jobs
are described under "Accounts and saved jobs" at the end.

**Status: v1, real data as of Phase 3.** `internal/job.Store` (Postgres-backed) is the
default implementation as of Phase 3; `internal/job.MockRepository` still exists and is
usable via a config flag for local frontend development without a database. `internal/api`
depends only on the `job.Repository` interface either way — swapping which implementation
`cmd/aggregator/main.go` constructs never touches a handler, route, or the frontend.

Only jobs currently `open` on their ATS board are ever returned; a job that disappears from
its board is closed out by `aggregator ingest` and stops appearing here (its detail URL
becomes a `404`). Real jobs have no tags yet (`tags` is always `[]`, and a `tag` filter
therefore matches nothing) and no company logo (`company_logo_url` is always `null`) —
neither exists in Greenhouse's public data; `region_note` carries the raw location string.

`remote_type` and `employment_type` both include an `unknown` value: Greenhouse's public Job
Board API (Phase 3's only source today) has no field for either, so every ingested job
starts as `unknown` on both until a later phase (geographic eligibility & relevance
filtering) classifies it from raw location/title text. This is not a placeholder that will
be removed — some jobs may stay genuinely unknown even after classification runs (e.g. a
raw location string too ambiguous to classify confidently), so `unknown` is a real,
permanent value, not a temporary one.

**Job age limit.** Jobs whose posting date (`posted_at`) is more than `JOB_MAX_AGE_DAYS` days ago
(default 15, `0` = no limit) are not returned by either endpoint: they are missing from
`GET /api/v1/jobs` (and from `total`) and `GET /api/v1/jobs/{id}` answers `404` for them. The
rows stay in the database, so raising the limit shows them again. A job with no publish date
counts from the day it was first seen. The mock repository (`JOB_REPOSITORY=mock`) does not apply
the limit.

**Application deadline.** A job whose deadline (`jobs.expires_at`, set by sources that publish
one: Ethiojobs, careers pages with `validThrough`) has passed is hidden the same way, whatever
`JOB_MAX_AGE_DAYS` says. A job with no deadline is never hidden by this rule.

**Priority companies.** Jobs from the curated Ethiopian tech companies
(`configs/ethiopian_companies.json`, see `docs/priority-companies.md`) carry
`"is_priority": true`. They get no special position: `GET /api/v1/jobs` is always sorted
newest first by `posted_at` (ties by id), whatever the company, and a priority job's age decides
where it lands. `priority=true` narrows a list to just those companies. Additive change:
existing consumers that ignore the new field and parameter see the same shapes as before.

**Source and credit.** Every job reports `"source"`, the provider it was collected through:
an employer's ATS (`greenhouse`) or a job board (`himalayas`, `remotive`, `remoteok`, `jobicy`,
`weworkremotely`, `workingnomads`, `ethiojobs`, `linkedin`). For a job board, `application_url` is
the board's own page for the job and the board asks to be credited; the UI prints "via <board>"
(see `docs/remote-boards.md`). Additive.

**Markets.** Every job belongs to one of two lists, reported as `"market"`: `"ethiopia"` (jobs
in Ethiopia or from Ethiopian employers: the Ethiopian category in the UI) or `"worldwide"`
(companies hiring across borders: the main page). The market comes from the source the job was
collected through, so one company can appear in both lists; the exception is a priority (Ethiopian)
company, whose jobs are always `"ethiopia"`, so they are never in the worldwide list. `market=ethiopia` or
`market=worldwide` narrows a list to one; omitting it returns both. Additive change: the field
is new and the parameter is optional.

**Eligibility and role.** Every job reports two objects that are `null` until the job has been
classified (`aggregator classify`, or `ingest`, which classifies what it stored):

- `eligibility`: whether a candidate living in Ethiopia could take the job without relocating.
  `status` is `eligible`, `ineligible` or `uncertain` (never a boolean: "the text does not say" is
  its own answer), `confidence` is 0..1, `basis` says what the verdict rests on, `restrictions`
  lists the limits found (`["United States only"]`, `[]` when none) and `hours_constraint` is a
  string when an eligible job asks for working hours far from Ethiopia's (US hours), else `null`.
  Bases: eligible: `local_ethiopia`, `worldwide`, `region_includes_africa` (EMEA, Africa),
  `timezone_window`, `exclusion_list_ok` (anywhere except some places), `explicit_permission`;
  ineligible: `restricted_places`, `excluded`, `timezone_outside`, `onsite_elsewhere`,
  `work_authorization`; uncertain: `conflicting_signals`, `no_signal`.
- `role`: `family` (`software_engineering`, `data_ml`, `devops_security`, `product_design`,
  `it_support`, `other_tech`, `non_tech`, from the title) and `relevant`, whether the role profile
  counts the family as relevant (the built-in profile: software, data, DevOps and security; replace
  it with `RELEVANCE_PROFILE`, see `docs/eligibility.md`).

The single-job response's `eligibility` adds `reasons` (one sentence each), `evidence` (the text the
verdict rests on, `{field, text}`), `locations` (places found) and `classifier_version`. How the
verdicts are made, and how good they are: [eligibility.md](eligibility.md). Additive: existing
consumers that ignore the new fields see the same shapes as before.

Base URL (dev): `http://localhost:8080/api/v1`

## GET /api/v1/jobs

List/search/filter jobs.

Query params (all optional):

| Param | Type | Notes |
|---|---|---|
| `q` | string | Case-insensitive literal substring of the title or company name (`%`/`_` match themselves, not as wildcards). The mock repository also matches tags; real jobs have none yet |
| `remote_type` | string | One of `remote`, `hybrid`, `onsite`, `unknown` |
| `employment_type` | string | One of `full_time`, `part_time`, `contract`, `internship`, `unknown` |
| `company` | string | Exact company name match |
| `tag` | string | Job must have this tag |
| `market` | string | `ethiopia` or `worldwide` returns only that list; omitted returns both. Anything else is a `400` |
| `priority` | string | `true` returns only jobs from priority (Ethiopian) companies. `false` is the same as omitting the parameter (it does **not** mean "only non-priority"), so a UI toggle can send its state verbatim. The web UI does not use this parameter (it splits by `market`); it stays for API clients. Anything else is a `400` |
| `eligibility` | string | Comma-separated statuses to keep: `eligible`, `ineligible`, `uncertain`, `unclassified` (jobs with no verdict yet). `eligibility=eligible,uncertain` keeps jobs that are open or unclear. Anything else is a `400` |
| `relevant` | string | `true` keeps roles the role profile counts as relevant. `false` is the same as omitting it. Anything else is a `400` |
| `role_family` | string | One family (`software_engineering`, ...). Lower-case letters and underscores, at most 40; anything else is a `400` |
| `page` | int | Default `1` |
| `page_size` | int | Default `20`, max `100` |

Response `200`:

```json
{
  "jobs": [
    {
      "id": "job_spotify_backend_eng",
      "title": "Backend Engineer, Payments",
      "company_name": "Spotify",
      "company_logo_url": null,
      "is_priority": false,
      "market": "worldwide",
      "source": "greenhouse",
      "remote_type": "remote",
      "employment_type": "full_time",
      "region_note": "Worldwide",
      "tags": ["go", "backend", "payments"],
      "posted_at": "2026-09-10T00:00:00Z",
      "application_url": "https://jobs.lever.co/spotify",
      "eligibility": { "status": "eligible", "confidence": 0.9, "basis": "worldwide", "restrictions": [], "hours_constraint": null },
      "role": { "family": "software_engineering", "relevant": true }
    }
  ],
  "page": 1,
  "page_size": 20,
  "total": 12
}
```

Invalid `remote_type`/`employment_type`/`priority`/`market`/`eligibility`/`relevant`/`role_family` values, or `page`/`page_size` out of range, are a
`400` (see Error shape), not silently ignored.

## GET /api/v1/jobs/{id}

Single job, full detail (`JobSummary` fields plus `description`).

Response `200`:

```json
{
  "id": "job_spotify_backend_eng",
  "title": "Backend Engineer, Payments",
  "company_name": "Spotify",
  "company_logo_url": null,
  "is_priority": false,
  "market": "worldwide",
  "source": "greenhouse",
  "remote_type": "remote",
  "employment_type": "full_time",
  "region_note": "Worldwide",
  "tags": ["go", "backend", "payments"],
  "posted_at": "2026-09-10T00:00:00Z",
  "application_url": "https://jobs.lever.co/spotify",
  "eligibility": {
    "status": "eligible",
    "confidence": 0.9,
    "basis": "worldwide",
    "restrictions": [],
    "hours_constraint": null,
    "reasons": ["the location open worldwide"],
    "evidence": [{ "field": "location", "text": "Worldwide" }],
    "locations": [],
    "classifier_version": 1
  },
  "role": { "family": "software_engineering", "relevant": true },
  "description": "Own the payments processing pipeline that powers Spotify Premium billing across 180+ markets..."
}
```

Response `404` if `id` doesn't exist (see Error shape).

## Error shape

Every non-2xx response:

```json
{ "error": "human-readable message" }
```

## CORS

Configurable via `CORS_ALLOWED_ORIGIN` (default `http://localhost:5173`, Vite's dev
port). With sign-in off, only `GET` is served and the job endpoints are read-only. With
sign-in on (see below), `CORS_ALLOWED_ORIGIN` must equal `PUBLIC_BASE_URL`; only that
origin is sent CORS headers and `Access-Control-Allow-Credentials`, and `*` is refused at
startup. `Retry-After` and `X-Request-Id` are both listed in
`Access-Control-Expose-Headers`, so a browser's `fetch` can read them (without this, a
server header a client relies on, like the real wait time on a `429`, is invisible to
`fetch` even though it is on the wire).

## Request ids, health and readiness

Every response has an `X-Request-Id` header, and the same id is on every log line of that request
(quote it when reporting a problem). A request id sent by the client is kept if it is at most 64 characters
of `A-Z a-z 0-9 . _ -`, otherwise replaced. A panic in a handler is logged with the id and the stack and
answered with a generic `500`.

`GET /healthz` is `200` while the process runs (liveness). `GET /readyz` is `200` when the database answers
a ping and `503` otherwise. Neither touches sign-in or mail, so a probe never sends an email.

A request body larger than the endpoint allows (4 KiB on `/auth/*`) is answered `413`.

## Accounts and saved jobs

Off unless `PUBLIC_BASE_URL` is set; design, deployment and privacy notes are in
[accounts.md](accounts.md). Sign-in is by an emailed single-use link, no password. The
session is an HttpOnly cookie (`__Host-session` over HTTPS, `session` on plain http for
development), so requests must be sent with credentials.

Every non-GET request to `/auth/*` or `/me*` must carry exactly one `Origin` header equal to
`CORS_ALLOWED_ORIGIN`, and either `Content-Type: application/json` (exactly that media type,
parameters such as `charset` aside) or a non-empty `X-Requested-With`; anything else is
refused with `403` and the code `cross_origin`. A missing `Origin` is always refused: a real
browser always sends one on a same-origin fetch. Every response under `/auth` and `/me`
(success, error, 404, 405, an `OPTIONS` preflight) is `Cache-Control: no-store` plus
`Vary: Cookie`, and every response from the API — these routes and the public job endpoints —
carries `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`,
`Content-Security-Policy: frame-ancestors 'none'` and `X-Frame-Options: DENY`.

Every error body is `{"error": "<text for a person>"}`, and on `/auth` and `/me` also
`"code"`, a stable identifier a client can branch on instead of matching the text:
`invalid_email`, `rate_limited` (429, with `Retry-After`), `mail_busy` (503, with
`Retry-After`), `invalid_link` (400: unknown, used or expired — deliberately not told apart),
`unauthorized` (401), `bookmark_limit` (409), `job_unavailable` (404), `cross_origin` (403),
`body_too_large` (413), `bad_request` (400), `unsupported_media_type` (415),
`confirmation_required` (400), `internal_error` (500, or 503 when the database is briefly
unreachable — never treated as signed out).

A request body over 1&nbsp;KiB on `/auth/*`, or a `GET`-only route's body of any size,
answers `413`.

| Method and path | Body | Success | Errors |
|---|---|---|---|
| `POST /api/v1/auth/login` | `{"email"}` | `202 {"status":"sent"}`, the same whether or not the address has an account | `invalid_email` 400, `rate_limited` 429 (`Retry-After`), `mail_busy` 503 (`Retry-After`) |
| `POST /api/v1/auth/inspect` | `{"token"}` | `200 {"email_masked":"a***@***.com"}`, does not spend the link | `invalid_link` 400, `rate_limited` 429 |
| `POST /api/v1/auth/verify` | `{"token"}` | `200 {"user":{"id","email"}}` and sets the cookie; spends the link and ends any session the caller already held | `invalid_link` 400, `rate_limited` 429 |
| `POST /api/v1/auth/logout` | none | `204`, clears the cookie | none |
| `POST /api/v1/auth/logout-all` | none | `204`, ends every session of the account (and cancels its unused links) | `unauthorized` 401 |
| `GET /api/v1/me` | none | `200 {"user":{"id","email"}}` | `unauthorized` 401 |
| `DELETE /api/v1/me` | `{"confirm": true}` | `204`, deletes the account, its sessions, saved jobs and unused links | `confirmation_required` 400 (body missing, not JSON, or `confirm` not `true`), `unauthorized` 401 |
| `GET /api/v1/me/bookmarks?page=&page_size=` | none | `200 {"jobs":[...],"page","page_size","total"}`, newest saved first | `unauthorized` 401 |
| `GET /api/v1/me/bookmarks/ids` | none | `200 {"ids":["12","34"]}` | `unauthorized` 401 |
| `PUT /api/v1/me/bookmarks/{id}` | none | `204`, idempotent | `unauthorized` 401, `job_unavailable` 404 (not found, or not currently visible: closed, expired or aged out), `bookmark_limit` 409 (over 500 saved) |
| `DELETE /api/v1/me/bookmarks/{id}` | none | `204`, idempotent | `unauthorized` 401 |

The emailed link is `<PUBLIC_BASE_URL>/auth/verify#token=...`. The token is in the URL
fragment so it never reaches a server log or a Referer header; the page reads it, asks
`/auth/inspect` who it is for, and only calls `/auth/verify` when the person presses a
confirm button, so a mail scanner that opens the link cannot spend it. Verifying ends
whatever session the browser making the request already held (never a stale session left
over from before signing in as someone else). A signed-in account may hold at most 10 live
sessions; verifying an 11th ends the oldest one first.

The session cookie is reissued (a fresh `Set-Cookie`, sliding its expiry) on `POST
/auth/verify` and, from time to time, on any authenticated request once more than half its
idle window has passed — a client that stores cookies normally needs no special handling for
this.

A saved job in `GET /me/bookmarks` is a normal job summary plus `closed` (the job is no
longer open on its board, or has passed its deadline, or has aged past `JOB_MAX_AGE_DAYS` —
all three read the same as "no longer listed") and `saved_at`. Saved jobs stay in the list
after they close. `GET /api/v1/jobs/{id}` also returns a job the signed-in account saved when
it is no longer publicly listed, with `"closed": true` on the detail (the field is omitted,
not `false`, on every public and every currently-listed job, so the public response shape is
unchanged); to anyone else, and to a job the caller never saved while it was visible, that job
stays `404`.
