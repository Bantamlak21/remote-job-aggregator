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
aggregator discover-boards --recheck              # re-look-up the boards already registered for the list's names
aggregator discover-boards --recheck --apply      # ... and deactivate (and close the jobs of) the ones refused as another company's
aggregator discover-boards - --from-boards --recheck --apply   # the same for boards found from employer names
aggregator ingest                                 # then read the boards it registered
```

A company's board is addressed by a slug that is nearly always its name, and each ATS answers 404
for a slug that does not exist, so a name is enough to look one up: the command tries the name run
together ("grafanalabs"), dash-joined ("grafana-labs") and without a trailing corporate word, on
all three ATSs, a few requests per name, 4 workers with a pause between requests. Companies that
already have an active board are skipped, so a rerun looks only at what is new. A probe is never
retried; one that fails (a 429, a server error, a cut-off body) is reported as failed, never as "not
found", and after 25 failed probes the run stops.

### Identity: a board is registered only on positive proof

The danger is another company using the slug. On the first live run, 6 of 178 registered boards
were exactly that (a "Wise" that is an insurer, a "Neon" that is a Brazilian bank, a "Ghost" that is
retail inventory). So the rules are strict, and everything refused is logged so a person can decide:

- **A domain in the names file is proof.** `Ramp | ramp.com`: the board is accepted if its job text
  (Lever and Ashby: the first eight listed jobs; Greenhouse: the first job) contains `ramp.com`, whatever
  the board calls itself. The domains in the list were each checked against a live board.
- **Otherwise, the name:** Greenhouse must name the board exactly as the company is named (ignoring
  `Inc`, `PLC` and a trailing `.io`/`.com` on the board's name; "Grafana" is not "Grafana Labs"). Lever
  and Ashby do not name themselves, so the company's name must appear as a whole word in at least half
  of the first eight listed jobs, on a board of at least two jobs. "Ramp" is not in "Trampoline", "Kit"
  is not in "Kitchen"; a name capitalized only at its start ("Ramp") is matched in that case, a name
  with capitals inside ("FullStory") in any case.
- **An everyday word is never accepted on its name.** "Close", "Wise", "Neon", "Warp", "Ghost",
  "Knock", "Axiom" and about 40 more (`commonWords` in `internal/discovery/guess.go`) are not looked up
  without a domain; the run logs them ("add `Name | domain`").
- **A name that verifies on more than one board is ambiguous** and refused, unless every one of those
  boards is proven by the domain.
- **"Customer.io" keeps its ending.** Its bare-word slug `customer` is tried but never accepted on the
  name alone.
- **For an employer a job board showed (`--from-boards`), a board proven only by name must also list one
  of that employer's job titles.** The job boards' own rows for the employer are the second witness: an
  insurer called Sentry, a "Ramp Agent" at an airline and a "Linear Accelerator Technician" all pass the
  name rule and share no title with the software company. The whole board's titles are compared (case and
  punctuation ignored); a board with a domain proof needs no titles. A board refused for this reason is
  reported as "lists none of the employer's job titles".
- Names with non-ASCII letters have no reliable slug and are not looked up. A board with no jobs is
  not registered.

The rules are judged against real boards: `internal/discovery/testdata/ownership` holds the first eight
listed jobs of 15 live boards (12 that are the company's, 3 that belong to another company). The test
`TestEval_OwnershipOnRealBoards` requires 100% precision (no wrong board registered) and at least 90%
recall, and logs both (100% and 100% on that sample). It is a small sample: the live run below is the
larger check.

Registered boards are worldwide targets; the market of an existing target is never changed. Boards
found from names skip the page probe that `discover` does (the ATS's own API is the proof).
`--from-boards` reads the employers of the remote job boards from the database, so the boards feed each
other: a company that Himalayas showed leads to its own, complete, board.
`--recheck` runs the same look-up over boards already registered. It changes something only on
**positive evidence**, which is narrow on purpose. A board is deactivated (with `--apply`, and its
jobs closed) only when all of these hold:

- `discover-boards` registered it (`discovery_metadata.source = "discover-boards"`); a board a person
  seeded is never touched;
- the look-up found the board and it **names itself as another company's** (a Greenhouse board named
  "Fin" for Intercom; Lever and Ashby boards do not carry a name, so no board there can be positive
  evidence of this kind);
- the company was fully checked (no failed probe, not left unreached).

Everything else that did not verify is not evidence and is only reported: a board that does not say
the company's name (most companies' jobs never do: Backblaze, Kinsta and Lucidworks are real and
unproven), one that shares no job title with a job board's few listings (Toptal, JumpCloud), a slug
that moved, a bad night. Those are logged as "left as it is" for a person to judge. Reports carry
structured refusals and failures (company, provider, slug, kind, reason), so a company name
containing `: ` reads back correctly.

## Measured (2026-09-25, live)

- The curated list has 404 names. Under the final rules, after the looser first pass was re-checked
  (42 of 179 boards deactivated, including the 6 known to be another company's), the list gives
  **210 companies with an active board** (111 Greenhouse, 84 Ashby, 15 Lever; 30 more boards are
  registered but inactive). Ten names got a domain (Buffer, Intercom, Mux, Thinkific, Chime, Nord
  Security, Doppler, Clerk, Outschool, Yugabyte): each was accepted only because its live board's job
  text carries the domain. Nine more domain guesses (HubSpot, Fly.io, Backblaze, Remofirst, Aha, Kinsta,
  Lucidworks, Tinybird, Lightspeed) were not found in any job of their boards and were left out of the
  list, so those boards stay unregistered until a domain their jobs do carry is found.
- `--from-boards` found boards among the employers the remote job boards showed. Requiring a shared job
  title also flagged the one wrong board among them (Anagram, a crypto research firm, taken for the
  security-training company a job board showed); it was deactivated by hand. Four real boards (Coalfire,
  Elsevier, JumpCloud, Toptal) share no title with the few listings a job board has and are left active.
- Ingesting the ATSs took about a minute for 111 Greenhouse targets. The worldwide list then held
  15,954 open jobs from 818 companies: 5,262 classified remote, 4,017 posted in the last 15 days.

## Limits

- **The 15-day age limit** (`JOB_MAX_AGE_DAYS`) applies here too. Ashby's `publishedAt` and
  Greenhouse's `first_published` are the original posting date, so a long-open role is hidden
  once it is older than the limit even though it is still on the board.
- **A slug that does not match the name is missed** ("Intercom" is board `intercom` but named
  "Fin"; "Nord Security" writes "Nord" in its jobs). A domain in the list, or a `discover` seed entry
  with the exact board, fixes it. **Boards with a single job are not registered** (too little to
  verify).
- **Lever's EU region** (`api.eu.lever.co`) is not read.
- **Identity is proven, not certain.** A different company that writes the same name in most of its
  jobs (and is not an everyday word) passes the name rule; for an employer a job board showed, the
  shared-title check catches it only when that job board's titles are on the board. A domain in the list
  is the fix. Measured precision is on 15 real boards plus synthetic look-alikes
  (`TestGuess_ANameProvenBoardMustListATitleTheEmployerListed`).
- **The title check costs recall.** An employer with one or two listings on a job board can have a real
  ATS board that shares none of them; it is then not registered (and never deactivated on that ground).
- **The 44 ATS jobs (of about 15,700) that repeat a title a job board already stores** for the same
  company are not merged: the board copies are skipped in a run, not closed when an ATS copy arrives.
- **Most jobs on a company board are not remote.** `remote_type` says which are (Greenhouse only
  when the location text says so); the UI's Remote type filter uses it.
- **Only three ATSs.** Workable, Recruitee, SmartRecruiters and others have public APIs and can
  be added the same way.
