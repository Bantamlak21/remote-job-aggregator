# Remote-first companies' own job boards

The worldwide list gets its jobs from remote job boards ([remote-boards.md](remote-boards.md))
and, more completely, straight from companies' own applicant tracking systems. Greenhouse, Lever
and Ashby publish every company's open jobs as public JSON with no key. Getting from "a company
that hires remotely" to "its board" is what this page is about.

## The clients

| Provider | API | What a job carries |
|---|---|---|
| `greenhouse` | `boards-api.greenhouse.io/v1/boards/{slug}/jobs?content=true` | title, location text, first-published date, description; **remote** or **hybrid** when the location text says so ("Remote, Bangalore") |
| `lever` (`internal/ats/lever`) | `api.lever.co/v0/postings/{slug}?mode=json` | title, categories (location, commitment), `workplaceType` (remote / hybrid / on-site), created date, description plus its lists |
| `ashby` (`internal/ats/ashby`) | `api.ashbyhq.com/posting-api/job-board/{slug}` | title, location and secondary locations, `workplaceType` / `isRemote`, employment type, published date, description; jobs marked unlisted are left out |

All three are per-company boards: a job missing from a board's answer is closed (unlike the
job-board collectors, which see only a sample). A missing board is `ErrBoardNotFound` (a 404), a
board with no openings is a valid empty list, and a response in another shape is an error, never
an empty list (that would close every stored job). One odd posting costs only that job. A job
whose URL is not on the ATS's own host is refused.

`remote_type` and `employment_type` come from what the ATS says and reach the jobs table;
a source that says nothing never resets a stored value.

## Finding boards from names: `discover-boards`

```bash
aggregator discover-boards                        # the curated list, configs/remote_companies.txt
aggregator discover-boards my-names.txt           # or your own list, one name per line
aggregator discover-boards - --from-boards        # every employer a job board showed that has no ATS board yet
aggregator discover-boards --from-boards --limit=300
aggregator ingest                                 # then read the boards it registered
```

A company's board is addressed by a slug that is nearly always its name, and each ATS answers 404
for a slug that does not exist, so a name is enough: the command tries the name run together
("grafanalabs"), dash-joined ("grafana-labs") and without a trailing corporate word, on all three
ATSs, a few requests per name, 4 workers with a pause between requests. It never retries a probe.

**A board is registered only when it proves it is the company's.** The danger is another company
using the slug ("signal", "front", "close"):

- Greenhouse names its board: the name must match the company's (ignoring `Inc`, `PLC` and a
  trailing `.io` / `.com`), so `intercom` (a board named "Fin") and `remote` (General Assembly's
  "Remote Jobs") are refused.
- Lever and Ashby do not name themselves, so the company's name must appear in the text of at
  least half of the board's first eight jobs.
- A board with no jobs is not registered (nothing to ingest and nothing to check).

Boards that exist but fail the check are logged as "not registered" so they can be looked at.
Registered boards are worldwide targets; the market of an existing target is never changed.
`--from-boards` reads the employers of the remote job boards from the database
(`company.Store.NamesWithoutBoard`), so the boards feed each other: a company that Himalayas
showed leads to its own, complete, board.

## Measured (2026-09-25, live)

- The curated list has 404 names: 171 companies had a verified board (180 boards; 8 more were
  refused by the identity check), and 178 of them registered (two boards' public pages did not
  answer the follow-up check).
- `--from-boards --limit=300`: 41 of 300 employers shown by the remote job boards had an ATS
  board (42 boards; 3 refused).
- Ingesting the boards took about 40 seconds for 181 targets and stored 9,965 new jobs. The
  worldwide list then held about 11,800 open jobs from about 780 companies, of which about 3,700
  were classified remote and about 3,200 were posted in the last 15 days.

## Limits

- **The 15-day age limit** (`JOB_MAX_AGE_DAYS`) applies here too. Ashby's `publishedAt` and
  Greenhouse's `first_published` are the original posting date, so a long-open role is hidden
  once it is older than the limit even though it is still on the board.
- **A slug that does not match the name is missed** ("Intercom" is board `intercom` but named
  "Fin"; "Nord Security" writes "Nord" in its jobs). Adding the name the company uses, or
  a `discover` seed entry with the exact board, fixes it.
- **Identity by name is a strong signal, not a proof.** A different company that writes the same
  name in most of its jobs would pass; the check makes that unlikely, not impossible.
- **Most jobs on a company board are not remote.** `remote_type` says which are (Greenhouse only
  when the location text says so); the UI's Remote type filter uses it.
- **Only three ATSs.** Workable, Recruitee, SmartRecruiters and others have public APIs and can
  be added the same way.
