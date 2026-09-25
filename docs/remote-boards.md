# Worldwide remote jobs: the remote job boards

The main page (the worldwide list, see [fresh-jobs.md](fresh-jobs.md#markets-ethiopian-category-and-worldwide-main-page))
gets its jobs from two kinds of source: companies' own ATS boards (Greenhouse) and six public
remote-job boards read through the free APIs and RSS feeds they publish for this use.

| Board | Provider | Where | Jobs per run (2026-09-25) | Asks for | Minimum gap between runs |
|---|---|---|---|---|---|
| Himalayas | `himalayas` | `himalayas.app/jobs/api` (cursor paged, 20 a page) | up to 1,000 (50 pages; 100,000 in the feed) | visible link back and a credit; data refreshes every 24 h; rate limited | 12 h |
| Remotive | `remotive` | `remotive.com/api/remote-jobs` | 19 (its free feed is a delayed sample) | link back to its URL and a credit; do not pass its jobs to other job sites; at most 4 requests a day | 6 h |
| Jobicy | `jobicy` | `jobicy.com/api/v2/remote-jobs?count=100` | 100 | a credit with a direct link; send applications to its job URL | 1 h |
| We Work Remotely | `weworkremotely` | `weworkremotely.com/remote-jobs.rss` | about 84 | robots.txt allows the feed; each item links to its own page | 1 h |
| Working Nomads | `workingnomads` | `workingnomads.com/api/exposed_jobs/` | about 57 | the feed it publishes for this use | 1 h |
| Remote OK | `remoteok` | `remoteok.com/api` | 100 | link back with follow, a credit, never its logo | 1 h |

`internal/ats/remoteboards` has one client per board. All are `ingestion.Collector`s in the
worldwide market, so an employer becomes a company and a worldwide target on first sight.

## What a board's job carries

- **The board's own page is the job's URL, enforced in code.** Every board asks for a link back,
  so `application_url` is the board's page for the job, not the employer's: a record whose URL is
  not `https` on the board's own host is refused (Himalayas builds it from the listing's `guid`,
  never from its `applicationLink`). The job detail page
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

- One request per board per run, except Himalayas: one per page, 0.5 s apart, stopping when a
  whole page lies outside the age window, at `HIMALAYAS_MAX_PAGES` (default 50, max 100), or at
  the end of the feed. The log line `himalayas: read` gives the pages, jobs and the oldest
  publish time reached.
- **A request is never retried.** A 429, 5xx or any non-200 is reported. Himalayas keeps the jobs
  an earlier page collected and reports the failure (`ats.ErrPartialResult`: the ingester stores
  the jobs and still lists the failed run in its results); the other boards fail alone.
- **Each board has a minimum gap between runs**, remembered in the `collector_runs` table
  (migration `000005`), so it holds across separate `ingest` processes and cron runs: a board
  that ran more recently is skipped with a log line, unless `--force` is given. An attempt counts
  (the request was made whether or not it succeeded), and if the run log cannot be read (a
  missing migration, a database problem) the board is not called and the run reports a failed
  result. A scheduled run may arrive up to 10 minutes short of the gap and still goes ahead, so
  an exact 12-hour cron is not skipped. Remotive's 6 hours keeps it near its 4 requests a day.
- Responses are size-capped (8 MiB); one over the cap is an error, never a shorter list.
- A response that decodes but yields **no usable record** is an error, never "no jobs" (a
  renamed field would otherwise age out every stored job); an empty result because every job is
  simply too old is fine. One odd record (a field of an unexpected type, a date that cannot be
  read) costs only that job. A date that is present but unreadable is not kept: it cannot be
  shown to be fresh.
- A collector's jobs close only after 14 days unseen (a feed shows only the newest jobs). An item
  already past its expiry (We Work Remotely, Himalayas) is not stored; a stored one is hidden by
  the API once its date passes.
- If an employer is renamed on a board, the job moves to the new spelling's company instead of
  being stranded (`job.Store.MoveJob`, used only by these many-employer sources).
- The documented APIs are read directly, not through robots.txt gating: they are the boards'
  own machine interface. No board page is crawled.

## How the terms are met

Every board asks for a visible credit and a link back; Remotive and Himalayas add conditions.

- **Credit and link back:** each job's URL is the board's own page (enforced), the API reports the
  `source`, and the UI prints "via <board>" on the card, "Apply on <board>" on the button and
  "Listing from <board>" with a link on the detail page.
- **Not passed on (Remotive):** the project sends these jobs nowhere: there is no job feed,
  sitemap or `JobPosting` markup, and nothing submits them to other job sites or search engines'
  job products. **Its public JSON API (`GET /api/v1/jobs`) is unauthenticated, however, and
  returns board jobs, Remotive's included, to anyone who calls it.** That is how the site's own
  pages read them, and Remotive's terms name resubmission to other job sites, not this; but an
  API that anyone can read is also a way to pass the jobs on. If you want to close that door,
  either leave Remotive out of the providers (`ingest` without it) or add an allow-list to the
  API; this is left as an owner decision.
- **Nothing collected in exchange:** the site asks for no sign-up or e-mail to see a job.
- **Rates:** the minimum gaps above.
- **Logos:** none stored or shown.

## Running it

```bash
aggregator ingest                                 # includes all six boards (free)
aggregator ingest --providers=remote-boards       # only the six boards
aggregator ingest --providers=himalayas,jobicy    # or any of them by name
aggregator ingest --providers=remotive --force    # ignore the minimum gap (use sparingly)
```

Run it about twice a day. A board asked for again inside its minimum gap is skipped, so running
more often is harmless; `--force` overrides that. Order: Ethiopian sources first, then
Himalayas, Remotive, Jobicy, We Work Remotely, Working Nomads and Remote OK, so an opening
several boards list (same employer, title and place) is stored from the one with the fullest
text.

## Limits

- **Most jobs are country-restricted.** Measured on the first live run: of 760 jobs stored, about
  120 were open to "Worldwide" or "Anywhere"; Himalayas alone had 252 of 500 restricted to the
  United States. The list does not yet filter by where a candidate may live; the region note on
  each card says. Eligibility filtering is Phase 4 of the roadmap.
- **A board is a sample.** Remotive's free feed is 19 jobs, Working Nomads' 57. Himalayas gains
  about 850 jobs a day: on 2026-09-25 its 50 pages (1,000 jobs) reached back about 28 hours, so
  even one run a day leaves no gap (the `himalayas: read` log line shows the oldest publish time
  reached; if it approaches the run interval, raise `HIMALAYAS_MAX_PAGES`). Companies' own boards (Lever, Ashby, Greenhouse) are far larger.
- **Duplicates across boards** are skipped by employer and title alone (every listing is remote
  and boards word the region differently: "USA", "United States"), both within one run and
  against what other sources already store, so boards that run in different invocations do not
  show one job twice. Different markets are never compared, and in the Ethiopian market the place
  must match too. If a copy is stored first from a poorer source, it stays until it ages out.
- **Priority companies:** a worldwide board's employer is matched only against priority companies
  that also hire outside Ethiopia (Gebeya), so a same-named company elsewhere does not take the
  Ethiopian company's row and badge.
- **Source text quirks are kept**: Remote OK's feed has mis-encoded characters in some titles
  ("MecÃ¡nico"), and its `location` field is often empty or has a trailing comma.
- **Employer names** come from each board's text. Two boards may spell one company differently
  (with and without "Inc", say), which gives two company rows.
