# Priority companies (Ethiopian tech)

Jobs from 25 curated Ethiopian tech companies are pinned above every other job on the board,
badged "Ethiopian company", and can be shown alone with a filter. This page says how those
jobs are found, what is checked before a job is trusted, what coverage to expect, and how to
run and change it.

## What the user sees

- `GET /api/v1/jobs` returns priority jobs first, on every page, regardless of date (newest
  first inside each group). Each job carries `is_priority`. `?priority=true` returns only
  priority jobs. Contract: [api.md](api.md).
- The web UI (`remote-job-aggregator-web`) shows an "Ethiopian company" badge, a violet card
  outline, and an "Ethiopian companies only" toggle next to the other filters.

## The list

`configs/ethiopian_companies.json`, one entry per company:

| Field | Meaning |
|---|---|
| `name` | Canonical name. Becomes the `companies` row and the board id of the company's search target |
| `aliases` | Other names the company posts under (`Kifiya Financial Technologies`). Only add names the company is known to use: aliases widen what search results are accepted as this company |
| `website` | Only when verified. Not guessed |
| `feeds` | RSS feed URLs listing openings |
| `career_pages` | Pages on the company's own site that link to its job postings |
| `hires_outside_ethiopia` | `true` only for a company that also posts jobs located in other countries (Gebeya: Nairobi, Lagos, Dakar) |

Loading rejects unknown fields (a typo like `carreer_pages` would otherwise silently drop a
source), duplicate names, and any URL that is not absolute http(s) or is claimed by two
companies. To add a company: add an entry, run `aggregator seed-priority`. To stop treating
one as priority: `UPDATE companies SET is_priority = false WHERE name = '...'` (seeding never
un-flags, so removing a line from the file does not).

## How jobs are found

Ethiopian tech companies are not on Greenhouse, Lever or Ashby (checked: none of the 25 has a
board), so the ATS clients cannot reach them. Their jobs come from three places, all fitted
into the existing model (a `target_companies` row per source, `jobs.source` = the source's
`ats_provider`):

| Provider | Board id | Source | Removal rule |
|---|---|---|---|
| `feed` | feed URL | RSS 2.0 (`internal/ats/feed`) | job missing from the feed is closed |
| `careers-site` | careers page URL | same-site links below that page (`internal/ats/careers`) | job missing from the page is closed |
| `search` | company name | Serper search over LinkedIn and Ethiojobs (`internal/ats/jobsearch`) | closed only after **21 days** unseen (a search shows a sample, not the whole board) |

Every company gets a `search` target; feeds and career pages are added where a working one was
found.

### What is checked before a job is trusted

Company sites and search results are full of stale and look-alike data, all of it observed
live while building this (2026-09-24):

- **Stale postings.** Kifiya's own careers site still lists postings from February 2025;
  EthSwitch's and ZalaTech's RSS feeds end in 2024 and 2023; 12 of the 13 Ethiojobs postings
  sampled were closed. Feeds and career pages drop anything dated older than 120 days, and
  anything whose `validThrough` has passed (Zare's five listed postings all expired in July,
  so none is shown). An Ethiojobs page is accepted only when the site itself says
  `status: "active"` and its expiry is in the future.
- **Look-alike companies.** A search for "Chapa" returns Chapa De Indian Health, Chapa Tax
  Business Solutions and a Mexican consultancy; "Gebeya" returns Chaka Gebeya. A LinkedIn
  result is accepted only if its URL slug ends in `-at-<company>-<id>` and `<company>` equals
  a configured name after dropping corporate suffixes (`Inc`, `PLC`, `S.C.`). An Ethiojobs
  page's own company field must equal one too.
- **Same-name companies in another country.** "Finance Executive at DreamTech" in Noida,
  India passed the slug check as the Ethiopian DreamTech. A LinkedIn result must therefore
  also show an Ethiopia signal (`et.linkedin.com`, or "Ethiopia"/"Addis Ababa" in its text),
  unless the company is marked `hires_outside_ethiopia`.
- **Links that are not jobs.** Only same-site links below the listing's path count; links to
  company-information pages (`culture`, `benefits`, `departments`, ...) and dead links (404) are
  dropped. This is a heuristic: a site with an unusual info-page name under its careers path
  can produce a non-job entry until that name is added to the skip list in
  `internal/ats/careers`. A query string that identifies a job (`/careers/job?id=7`) is kept;
  tracking parameters (`utm_*`, `ref`, ...) are not.
- **Unreadable feed dates.** A feed item whose date is present but cannot be parsed is dropped
  (it cannot be shown to be fresh); an item with no date at all is kept.
- **A page that stopped listing jobs.** A careers page with no candidate links is an error
  (`careers: no job links found`), never "zero openings", because a redesigned or
  script-rendered page looks the same and would otherwise close every open job. The cost: a
  company whose last opening is removed keeps showing it until someone looks at the error.

### Rules the fetchers follow

- **robots.txt (RFC 9309)** is checked before every page and feed fetch
  (`internal/robots`): longest-match, `Allow` wins ties, wildcard/`$` patterns, a group naming
  exactly `remote-job-aggregator` over `*`, a BOM and CR / CRLF / LF line endings accepted,
  percent-encoding normalized before matching. A robots.txt that cannot be read (5xx, 429,
  network error) or fully parsed means the URL is not fetched. **Redirects are checked too**:
  each hop must stay on the same site and land on a URL robots.txt allows, and is refused
  before it is requested (an allowed URL cannot launder a request to a disallowed one).
  Ethiojobs allows job pages and disallows `/api/*`, which is never used.
- **LinkedIn pages are never fetched.** Its robots.txt disallows crawling and job pages are
  behind a login wall. A LinkedIn job comes only from the search result (URL, title, snippet,
  date); the job's link sends the person to LinkedIn to apply.
- **Bounded, and never silently cut**: a page over 2 MiB, or a careers listing with more than
  100 candidate links, is an error (`ErrTooLarge`, `ErrTooManyJobs`), not a shorter list,
  because a full source closes whatever it does not see. Detail pages are fetched 500 ms
  apart; at most 10 Ethiojobs pages per company per run.
- **Serper budget**: two queries per company per run (LinkedIn, Ethiojobs), month-limited,
  serialized, one retry on a transient failure. One call is exactly one request on the wire
  (the shared client's own retries are switched off for these calls), so the budget counts
  what Serper bills. `SEARCH_MAX_QUERIES_PER_RUN` (default 60) caps the run; hitting it fails
  the remaining companies with `ErrBudgetExhausted` instead of spending more. The free tier is
  a fixed 2,500 queries, so a weekly run of the search source (50 queries) lasts about a year.
- **Ended jobs close at once.** When a search result or an Ethiojobs page says a job has ended
  (`status: closed`, past expiry, "No longer accepting applications"), ingestion closes any
  stored copy immediately rather than waiting out the 21-day window.
- **Future publish dates are ignored** (more than 24 h ahead of now): a source bug must not pin
  a job above every real one.

## Running it

```bash
aggregator migrate-up          # 000002 adds companies.is_priority
aggregator seed-priority       # 25 companies + their sources; safe to repeat
aggregator ingest --providers=feed,careers-site   # cheap sources, run daily
aggregator ingest --providers=search              # 50 Serper queries, run weekly
aggregator priority-report     # coverage: open jobs per company per source
```

`aggregator ingest` with no `--providers` runs the free sources (Greenhouse, feeds, careers
pages) and **never** the search source: it spends a fixed, non-renewing Serper allowance, so a
plain daily run would exhaust the free 2,500 queries in about 50 days. The search source runs
only when named (`--providers=search`, or `--providers=greenhouse,feed,careers-site,search`
for everything). Naming it without `SERPER_API_KEY` is an error, not a silent no-op. After every `ingest` the run logs
`priority coverage companies_with_open_jobs=N priority_companies=25 open_priority_jobs=M`.

Restart: none needed for `ingest`/`seed-priority`/`priority-report` (they are one-shot).
`aggregator serve` must be restarted to pick up the new `is_priority` field and the
`priority` parameter.

## Coverage today (measured 2026-09-24, live)

`priority-report` after seeding and one full `ingest` run: **4 of 25 companies had an open
job (8 jobs)**: EthSwitch 4, Addis Software 2, Kifiya 1, Zare Innovations 1. The run made 50
Serper queries (limit 60) and all 34 targets succeeded. That is an honest reading of what is publicly posted right now, not a
ceiling on the code:

- 11 of the 25 companies have no career page or feed that could be found at all (Wizard Labs,
  4Africa Systems, Lelna AI, Ahun, Dalol Web Services, Cybersoft PLC, ZEMEL IT SYSTEMS PLC,
  Parallel Solutions, 251 Technologies, Hisab Technologies, ERP Solutions PLC). They are
  covered only by search, and search found nothing current for them.
- Where a company posts, it mostly posts on Ethiojobs and LinkedIn, not on its own site.
  EthSwitch posted three roles on Ethiojobs in the last week; its own feed is two years old.
- Companies with pages that are not yet crawlable: Chapa, Ashewa Technology, EagleLion,
  Gebeya, DreamTech (their careers pages showed no job links to the crawler). Adding a
  `career_pages` entry is one line once one is confirmed to work.

The number to watch is the `priority coverage` log line and `priority-report`; if it drops
after a run, look at `target not ingested` warnings first.

## Limits

- **Search is a sample.** Serper returns 10 results per query; a company with more than 10
  matching jobs in a month shows some of them. Closing after 21 days means a job that was
  filled can stay visible for up to 3 weeks after it stops being seen.
- **One job posted on both sites** is stored once per run (same title, case-insensitive, and
  the same place), keeping the Ethiojobs version. "Same place" is coarse: an empty location or
  one naming Ethiopia/Addis Ababa counts as Ethiopian, anything else only matches an equal
  location. The same title in Addis Ababa and Nairobi stays two jobs; two different openings
  with the same title in Addis Ababa collapse into one.
- **A name is not an identity.** `nameKey` drops trailing corporate words (`Inc`, `PLC`,
  `S.C.`, `Co`, `Ethiopia`), so "Chapa Ethiopia" is accepted as Chapa.
- **Dates from search results are approximate** ("3 days ago" is converted to a timestamp),
  so `posted_at` for LinkedIn jobs is a best estimate.
- **Location** for LinkedIn jobs is read from the result text and is empty when the text has
  none. Classifying remote/onsite and eligibility is Phase 4.
- **Company-name collisions** are reduced, not eliminated: a same-named company that also has
  an Ethiopian location would pass. The configured names and aliases are the only lever.

## Tests

Unit tests use fakes and real result/page shapes captured from the live sites. Postgres tests
(ordering across pages, the priority filter, `CloseStale`, `CloseBySourceID`, migration
up/down, seeding the real 25-company file, the coverage report) need `TEST_DATABASE_URL` and
`go test -p 1`. The redirect and request-metering guarantees are tested through the real
`httpclient` and `robots.Checker` against `httptest` servers, not fakes.
