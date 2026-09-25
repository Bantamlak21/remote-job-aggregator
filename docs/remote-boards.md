# Worldwide remote jobs: the remote job boards

The main page (the worldwide list, see [fresh-jobs.md](fresh-jobs.md#markets-ethiopian-category-and-worldwide-main-page))
gets its jobs from two kinds of source: companies' own ATS boards (Greenhouse) and six public
remote-job boards read through the free APIs and RSS feeds they publish for this use.

| Board | Provider | Where | Jobs per run (2026-09-25) | Asks for |
|---|---|---|---|---|
| Himalayas | `himalayas` | `himalayas.app/jobs/api` (cursor paged, 20 a page) | up to 500 (25 pages; 100,000 in the feed) | visible link back and a credit; data refreshes every 24 h; rate limited |
| Remotive | `remotive` | `remotive.com/api/remote-jobs` | 19 (its free feed is a delayed sample) | link back to its URL and a credit; do not pass its jobs to other job sites; at most 4 requests a day |
| Jobicy | `jobicy` | `jobicy.com/api/v2/remote-jobs?count=100` | 100 | a credit with a direct link; send applications to its job URL |
| We Work Remotely | `weworkremotely` | `weworkremotely.com/remote-jobs.rss` | about 84 | robots.txt allows the feed; each item links to its own page |
| Working Nomads | `workingnomads` | `workingnomads.com/api/exposed_jobs/` | about 57 | the feed it publishes for this use |
| Remote OK | `remoteok` | `remoteok.com/api` | 100 | link back with follow, a credit, never its logo |

`internal/ats/remoteboards` has one client per board. All are `ingestion.Collector`s in the
worldwide market, so an employer becomes a company and a worldwide target on first sight.

## What a board's job carries

- **The board's own page is the job's URL.** Every board asks for a link back, so
  `application_url` is the board's page for the job, not the employer's. The job detail page
  says "Apply on Remotive" and prints "Listing from Remotive" with a visible link.
  The API reports each job's `source`, and the UI credits any job board with it
  (`remote-job-aggregator-web/src/utils/sources.ts`); jobs from an employer's own ATS have
  no credit line.
- **Remote type is `remote`**, the board's whole subject. Employment type comes from the
  board's own field when it has one (`full_time`, `part_time`, `contract`, `internship`;
  freelance and temporary count as contract).
- **Location text is what the board says about eligibility**, kept as the card's region note:
  Remotive's "candidate required location", Himalayas' location restrictions ("Worldwide" when
  it lists none), Jobicy's `jobGeo`, We Work Remotely's countries, Working Nomads' location.
- **Dates are the board's own**; jobs older than 20 days are left out (the API also hides jobs
  older than `JOB_MAX_AGE_DAYS`, 15 by default). We Work Remotely's expiry date is kept.
- **No logos are stored.** Remote OK forbids using its logo; the project has no logo column.

## Rates and safety

- One request per board per run, except Himalayas: one per page, 0.5 s apart, stopping at the
  first page older than the window, at `HIMALAYAS_MAX_PAGES` (default 25, max 100), or at the
  end of the feed.
- **A request is never retried.** A 429, 5xx or any non-200 is reported; the run keeps what an
  earlier page collected (Himalayas) or fails that board alone (the others keep running).
- Responses are size-capped (8 MiB); one over the cap is an error, never a shorter list.
- A response that decodes but lists nothing is an error, never "no jobs": an empty result
  would age out every job the board has stored.
- A collector's jobs close only after 14 days unseen (a feed shows only the newest jobs), or at
  once when the board reports them expired (We Work Remotely).
- The documented APIs are read directly, not through robots.txt gating: they are the boards'
  own machine interface. No board page is crawled.

## Running it

```bash
aggregator ingest                                 # includes all six boards (free)
aggregator ingest --providers=remote-boards       # only the six boards
aggregator ingest --providers=himalayas,jobicy    # or any of them by name
```

Run it about once a day. Remotive asks for at most four requests a day and Himalayas refreshes
every 24 hours, so more often only risks being blocked. Order: Ethiopian sources first, then
Himalayas, Remotive, Jobicy, We Work Remotely, Working Nomads and Remote OK, so an opening
several boards list (same employer, title and place) is stored from the one with the fullest
text.

## Limits

- **Most jobs are country-restricted.** Measured on the first live run: of 760 jobs stored, about
  120 were open to "Worldwide" or "Anywhere"; Himalayas alone had 252 of 500 restricted to the
  United States. The list does not yet filter by where a candidate may live; the region note on
  each card says. Eligibility filtering is Phase 4 of the roadmap.
- **A board is a sample.** Remotive's free feed is 19 jobs, Working Nomads' 57. Companies'
  own boards (Lever, Ashby, Greenhouse) are far larger.
- **Duplicates across boards** are removed within one run only (same employer, title, city).
- **Source text quirks are kept**: Remote OK's feed has mis-encoded characters in some titles
  ("MecÃ¡nico"), and its `location` field is often empty or has a trailing comma.
- **Employer names** come from each board's text. Two boards may spell one company differently
  (with and without "Inc", say), which gives two company rows.
