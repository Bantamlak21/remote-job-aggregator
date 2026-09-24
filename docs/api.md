# Public Job API (v1)

Read-only JSON API backing the job-board frontend. No auth — this is public data by
design (job listings are meant to be found).

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

**Priority companies.** Jobs from the curated Ethiopian tech companies
(`configs/ethiopian_companies.json`, see `docs/priority-companies.md`) carry
`"is_priority": true` and are pinned above every other job in `GET /api/v1/jobs`, on every
page, regardless of `posted_at`; within each group the order is newest first. `priority=true`
narrows a list to just those companies. Additive change: existing consumers that ignore the
new field and parameter see the same shapes as before.

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
| `priority` | string | `true` returns only jobs from priority (Ethiopian) companies. `false` is the same as omitting the parameter (it does **not** mean "only non-priority"), so a UI toggle can send its state verbatim. Anything else is a `400` |
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
      "remote_type": "remote",
      "employment_type": "full_time",
      "region_note": "Worldwide",
      "tags": ["go", "backend", "payments"],
      "posted_at": "2026-09-10T00:00:00Z",
      "application_url": "https://jobs.lever.co/spotify"
    }
  ],
  "page": 1,
  "page_size": 20,
  "total": 12
}
```

Invalid `remote_type`/`employment_type`/`priority` values, or `page`/`page_size` out of range, are a
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
  "remote_type": "remote",
  "employment_type": "full_time",
  "region_note": "Worldwide",
  "tags": ["go", "backend", "payments"],
  "posted_at": "2026-09-10T00:00:00Z",
  "application_url": "https://jobs.lever.co/spotify",
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
port). Only `GET` is served in v1 — read-only.
