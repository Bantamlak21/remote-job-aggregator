# Public Job API (v1)

Read-only JSON API backing the job-board frontend. No auth — this is public data by
design (job listings are meant to be found).

**Status: v1, mock data.** `internal/job.MockRepository` serves realistic fixture jobs.
Phase 3 (ATS ingestion) will implement a Postgres-backed `job.Repository` over the real
`jobs` table; `internal/api` depends only on the `job.Repository` interface, so swapping
the implementation in `cmd/aggregator/main.go` is the entire migration — no handler,
routing, or frontend change required.

Base URL (dev): `http://localhost:8080/api/v1`

## GET /api/v1/jobs

List/search/filter jobs.

Query params (all optional):

| Param | Type | Notes |
|---|---|---|
| `q` | string | Matches against title, company name, and tags (case-insensitive substring) |
| `remote_type` | string | One of `fully_remote`, `hybrid`, `onsite` |
| `employment_type` | string | One of `full_time`, `part_time`, `contract`, `internship` |
| `company` | string | Exact company name match |
| `tag` | string | Job must have this tag |
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
      "remote_type": "fully_remote",
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

Invalid `remote_type`/`employment_type` values, or `page`/`page_size` out of range, are a
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
  "remote_type": "fully_remote",
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
