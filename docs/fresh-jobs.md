# Fresh job sources: Ethiojobs and LinkedIn keyword search

Two sources collect the newest jobs from many employers at once, instead of asking about one
listed company at a time. Both are "collectors" (`ingestion.Collector`): they return jobs that
name their employer, and ingestion creates a company and a target for each employer on first
sight.

| Source | Provider | Where the jobs come from | Cost | Runs by default |
|---|---|---|---|---|
| Ethiojobs | `ethiojobs` | `ethiojobs.net/jobs?page=N`, the site's own newest-first listing | free | yes |
| LinkedIn keywords | `linkedin` | Google results (through Serper) for `site:linkedin.com/jobs/view "Ethiopia" <keyword>`, last day only | 1 Serper query per keyword | no, name it |

## Markets: Ethiopian category and worldwide main page

Every target has a `market` (`ethiopia` or `worldwide`, `target_companies.market`, migration
`000004`); a job's market is its target's. The API reports it as `"market"` and filters on
`?market=`; the UI shows `worldwide` on the main page and `ethiopia` under the Ethiopia tab.
The Ethiojobs and LinkedIn keyword collectors, the priority list's targets, and the old
`feed`/`careers-site`/`search` targets are `ethiopia`; ATS boards (Greenhouse, Lever, Ashby) and
every future worldwide source default to `worldwide`, even when the board belongs to a priority
company. The market is a property of the source, not the company, so one
company can appear in both lists. A collector names its market by implementing
`market.Provider`; a re-sighting that names no market never moves an existing target.

## Why these two

LinkedIn has no API that lets a third party read jobs, and its robots.txt disallows crawling
(`User-agent: * Disallow: /`), so this project never fetches a LinkedIn page. The only
LinkedIn data used is what a search engine shows in a result: URL, title, snippet, date.
Ethiojobs is the largest Ethiopian job board and publishes its listing to any browser; its
robots.txt allows `/jobs` and disallows only `/api/*`, which is never used.

## Ethiojobs (`internal/ats/ethiojobs`)

- Reads listing pages newest first, 12 jobs each, until a page's oldest job is past the age
  cutoff (20 days), the site's last page, or `ETHIOJOBS_MAX_PAGES` (default 100, max 200).
  The site had about 100 pages (about 1,200 jobs) on 2026-09-24. A full run takes about 90 seconds
  (pages are fetched 500 ms apart) and stored 944 jobs from 399 employers on the dev database.
- Each page embeds its data as JSON (`__NEXT_DATA__`): title, employer, description, location,
  published date and application deadline, all to the minute. So dates are exact, unlike
  LinkedIn's.
- A listing past its deadline is reported as ended and closes any stored copy at once.
  Listings with a hidden employer ("Confidential") are skipped, and the site's "Not Specified"
  location filler is stored as an empty location.
- Page 1 failing, or listing no jobs at all, is an error (an empty result must never look like
  "no jobs"). A later page
  failing ends the run with what was collected.
- Every fetch goes through the robots-gated page fetcher; redirects are checked too.
- Employers that match the priority list (`configs/ethiopian_companies.json`, by name and
  aliases, ignoring `PLC`, `S.C.` and similar) are attached to the existing priority company,
  so their jobs get the badge and the `?priority=true` filter. Every other employer becomes a
  new, non-priority company.

## LinkedIn keyword search (`internal/ats/jobsearch`, `FreshClient`)

- Keywords live in `configs/linkedin_queries.json` (24 today, 1 page each). Each keyword is
  one Serper query limited to results Google saw in the last day. Keywords are plain words:
  quotes, `OR`, `site:` and other operators are refused at load time so a keyword cannot widen
  the search.
- The employer comes from the URL slug (`<title>-at-<company>-<id>`). Result titles come in
  too many shapes to rely on (`X at Acme - LinkedIn`, `Acme hiring X in Y`, `X at Acme | ACME`,
  `X - LinkedIn Ethiopia` with no company). The title only supplies the company's real
  capitalization when a run of its words spells the slug; otherwise the slug's words are
  capitalized (so `iom-un-migration` becomes "Iom Un Migration"). Hidden employers
  ("Confidential") and names that are web addresses are dropped.
- **Ethiopia check.** Search text is noisy: a LinkedIn page lists similar jobs and even a phone
  country-code list ("Ethiopia+251; Falkland Islands+500") that mention Ethiopia for jobs
  elsewhere, and LinkedIn serves any job on any country subdomain. A first version that
  accepted any mention of Ethiopia stored a Zambian Coca-Cola job, a UK Live Nation job and a
  US ad-tech job on the first live run. Now only the job's own stated location counts: when the
  result gives one it must name Ethiopia or Addis Ababa (whole words: "Addison, TX" does not
  count); when it gives none the result must come from `et.linkedin.com`.
- Posting dates are estimated from the LinkedIn job id (see
  [priority-companies.md](priority-companies.md#limits)); Google's date is used only to reject.
- A result that says "No longer accepting applications" is reported as ended.
- The same opening posted twice (same employer, same title, same city) is stored once; a
  location that names no city ("Ethiopia") counts as the same city as any Ethiopian one.
- A priority company's jobs are filed under the `search` provider, the one the per-company
  source already uses, so one LinkedIn job found by both sources is one row.

Measured live on 2026-09-24: 24 queries returned 102 results, of which 16 became jobs after
the Ethiopia check (the rest were other countries or not job pages). That is a sample of
what LinkedIn shows Google, not every Ethiopian LinkedIn job.

## Serper budget

Serper's free tier is a fixed 2,500 queries, not a monthly allowance. Both Serper-backed
sources share one per-run budget, `SEARCH_MAX_QUERIES_PER_RUN` (default 60): the per-company
`search` source spends 25 (one per priority company), the keyword source about 24. Every
attempt counts, including a retry. When the budget runs out the keyword source keeps what it
found and stops; a rejected key stops it at once. A weekly run of both costs about 49 queries,
which lasts about a year of the free tier; a daily run of only the keyword source lasts about
100 days. Because of that, neither runs unless named.

## Removal and the `expires_at` column

A collector sees a sample (the newest N jobs), so a job missing from one run proves nothing.
Collector jobs are closed when they have not been seen for `StaleAfter` (14 days for
Ethiojobs, 21 for LinkedIn), or immediately when the source reports them ended. Employers
that did not appear in a run at all are aged out the same way. An employer seen only through
ended postings is never created as a company.

Run `ingest` right after `migrate-up`: until the direct Ethiojobs source has run, the
Ethiojobs postings that the old per-company search stored are closed and not yet replaced.

Migration `000003` adds `jobs.expires_at` (the application deadline). The API hides a job
whose deadline has passed, independently of `JOB_MAX_AGE_DAYS`, in the list, `total` and
detail. It also closes the old per-company Ethiojobs rows that the `search` source stored
(`source = 'search'` and `source_job_id LIKE 'ethiojobs:%'`), which the direct source
replaces. Rolling the migration back drops the column but does not reopen those rows.

## Running it

```bash
aggregator migrate-up                                  # 000003: jobs.expires_at
aggregator ingest                                      # free sources, includes Ethiojobs
aggregator ingest --providers=ethiojobs                # only Ethiojobs
aggregator ingest --providers=linkedin                 # ~24 Serper queries
aggregator ingest --providers=ethiojobs,linkedin,search
```

Collectors run after the per-company sources, Ethiojobs before LinkedIn, so an opening both
list is stored with Ethiojobs' full description rather than LinkedIn's snippet. `linkedin`
needs `SERPER_API_KEY` and `configs/linkedin_queries.json`; naming it without them is an
error. Restart `aggregator serve` after `migrate-up` (the API's queries use the new column).
No frontend change is needed: new employers appear without the priority badge.

## Limits

- **In-run duplicates only.** The same opening (employer, title, city) found by Ethiojobs and
  LinkedIn in one run is stored once. A copy stored by an earlier run, or by the per-company `search` source, is not
  compared, so a job can appear twice until one copy ages out.
- **Place matching is coarse and not transitive.** A location with no city ("Ethiopia") matches
  every Ethiopian city, and two named cities match only when equal. Inside one collector run
  the named-city jobs are settled first, so the result does not depend on the order search
  returns them; across collectors the earlier collector's jobs win, so a city-less LinkedIn
  copy is dropped as a duplicate of any Ethiojobs job with the same employer and title.
- **LinkedIn is a sample.** Google shows a few results per keyword for the last day; a job
  LinkedIn has but Google has not indexed yet is not seen. Add keywords to widen it (each costs
  one query per run), up to 40 per config. A config needing more queries than
  `SEARCH_MAX_QUERIES_PER_RUN` logs a warning at startup and the last keywords do not run.
- **Company names from slugs** lose acronym casing when the title does not repeat the name.
- **A name is not an identity.** Two different companies with one name share a row, as in the
  rest of the project.
- **Ethiojobs layout.** The source reads the site's embedded JSON. If the site changes its
  layout, page 1 fails with "no embedded data" and the run reports it instead of storing
  nothing silently.
