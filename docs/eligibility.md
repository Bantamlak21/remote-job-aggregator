# Eligibility and relevance (Phase 4)

Most remote jobs are not open to someone living in Ethiopia: a "remote" job is often limited to
one country, a region or a set of time zones. This phase decides, for every open job, two things,
deterministically (rules and tables, no model):

1. **Eligibility**: could a candidate living in Ethiopia (UTC+3) take this job without relocating?
   `eligible`, `ineligible` or `uncertain`, with a confidence, a reason, and the text it rests on.
2. **Relevance**: what kind of role is it (a *family*: software, data, DevOps and security,
   product and design, IT support, other tech, non-tech) and does the seeker's profile count that
   family as relevant?

Both are stored in `job_eligibility` (migration `000006`), served by the API on every job
(`docs/api.md`), and filterable: `?eligibility=eligible,uncertain&relevant=true`.

## Running it

```bash
aggregator migrate-up          # 000006 job_eligibility
aggregator classify            # jobs with no verdict, or one made by older rules / an older version of the job
aggregator classify --reclassify   # every open job again
aggregator ingest              # classifies what it just stored (a failure there does not fail the ingest)
```

A verdict is recomputed when the rules change (`filtering.Version`), when the job's content hash
changes, or when the target country changes. 21,338 jobs take about 26 seconds (8 workers, batches
of 200, memory bounded by the batch). A panic in the rules on one job stores that job as
`uncertain` with basis `no_signal` (it stays visible, is retried on the next rules version, and is
never marked eligible).

Which roles count as relevant is a JSON profile: `internal/filtering/relevance/profile.json` is the
built-in one (software, data, DevOps and security are relevant). To change it, copy the file, edit
the `relevant` list and the rules (ordered; the first rule whose phrase appears in the title as
whole words, and none of whose `not` phrases do, decides the family) and set
`RELEVANCE_PROFILE=/path/to/profile.json`, then `aggregator classify --reclassify`.

## What decides a verdict

Postings say where a job can be done in three places, and the rules read all three, in this order
(the first that decides wins):

1. **The job is in Ethiopia** (Addis Ababa, an Ethiopian region, "Ethiopia"; a job in the Ethiopian
   list with no place elsewhere): `eligible`, `local_ethiopia`, even if on-site.
2. **An explicit restriction** in the description, the title, or We Work Remotely's
   `Headquarters:` line beats a permissive location: "must be located in Germany", "you are based
   in Europe", "remote (within Germany)", "this role can be based anywhere within the UK",
   "candidates must be located in one of these regions" (list on the next lines), "for candidates
   located in Canada" (pay boilerplate: a hint, used only when the location says nothing),
   "Resident in France for the last 5 years", "location: San Francisco Bay Area (required)",
   "US citizenship is required", a US security clearance, a title of "(US based)", "Based in
   London" or a territory such as "UKI". A restriction whose places include Ethiopia (or a region
   that contains it) is not a restriction. "Worldwide, except residents of the US" is an exclusion,
   not a requirement.
3. **An explicit permission** in the description beats a narrower location, but only when it is
   about this role: "candidates may be based in any geography", "Location: Worldwide". A company-wide
   statement ("we hire globally", "teams in 75+ countries", "work from anywhere for 6 weeks a year")
   does not widen a country, and "this role can be remote anywhere within the UK" is a restriction.
4. **The location field.** For a job *board* (Himalayas, Remotive, Jobicy, We Work Remotely,
   Working Nomads, Remote OK) it is where applicants may come from: worldwide or a region that
   contains Ethiopia (EMEA, Africa, East Africa) is eligible; a list of countries or regions without
   it (Europe, LATAM, APAC, North America, "South Africa" the country) is not; "not in the US, CA,
   UK, NZ, AU" is eligible; a time-zone window is eligible when it contains UTC+3 (`CET (+/- 3
   hours)`). For an *ATS* (Greenhouse, Lever, Ashby) it is where the job is: "Home based - EMEA" is
   eligible, "Novi Sad, Serbia, EMEA" (EMEA as an office's region tag) is not, and neither is a
   region next to a country or city in a remote job ("Remote, Germany, EMEA").
5. **The workplace type** with a place: on-site or hybrid outside Ethiopia is `ineligible`; a bare
   country or state with no work mode is `ineligible` (the job is tied to it); an office *city*
   with no work mode is `uncertain` unless the description says, about this role, that it is
   office work ("This role is based in our Madrid office", "3 days a week in the office",
   `#LI-Onsite`). A company-wide "we operate as a hybrid workplace" says nothing about this job.
6. Otherwise **`uncertain`**: a bare "Remote" or "Hybrid", no location, or signals that disagree
   (`conflicting_signals`). A bare "Remote" is not by itself `eligible`: it needs a permission in
   the text (rule 3).

A restriction with a relocation escape ("or open to relocate", "other locations considered") is a
hint, not a requirement, and disagrees with a permissive location rather than overriding it. A
description that says the company hires in a region that includes Ethiopia disagrees with a
location that leaves it out.

An eligible job that asks for far-off working hours ("working US business hours (EST)") is still
eligible and carries `hours_constraint`; a job that requires the candidate to be *located* in US
time zones is `ineligible` (`timezone_outside`).

Every verdict has `basis` (see `docs/api.md`), `reasons`, `evidence` quoted from the posting, the
`locations` found and any `restrictions`. Confidence is 0..1: about 0.9 for an explicit statement,
0.6 to 0.8 for an inference, 0.3 for a conflict.

### Where the places come from

`internal/filtering/geo`: countries and their English names from `golang.org/x/text/language`
(ISO 3166 and CLDR), regions from the same package's UN M.49 data (Africa, Eastern Africa,
Sub-Saharan Africa, Europe, Americas...), so "does Africa contain Ethiopia" is data. On top of that:
aliases (USA, UK, UAE, Czech Republic, native names), job-post regions (EMEA, APAC, LATAM, MENA,
GCC, Nordics, DACH, UKI, the EU, the EEA), US states and Canadian provinces, about 400 large
cities and hubs, and time zones (`CET`, `UTC+2`, `Pacific Time`, "US time zones"...). Two-letter
codes count as countries only inside a list of places, after "not in", or after "remote"
("San Francisco, CA" is a state; "IN", "IT" and "OR" are words). A city name that is also a common
word is left out of the tables. Adding a place is a table entry and a test.

## How good it is

Quality cannot be captured by unit tests alone, so there is an eval on real postings, scored
against labels made by an independent labeler who never saw the code, from a rubric written
before the code (definitions, 20 edge-case policies, thresholds).

| Set | What | Result |
|---|---|---|
| dev (325 real jobs, `internal/filtering/testdata/dev.jsonl`) | the rules were built against it | accuracy 0.945; eligible precision 1.000, recall 1.000; ineligible precision 0.969, recall 0.952; uncertain share 0.154; role family accuracy 0.932, relevance precision 1.000, recall 0.970 |
| audit (400 jobs the rules had not seen, a second labeler; bugs found here were fixed) | sampled by prediction: 160 predicted eligible, 150 ineligible, 90 uncertain | 0 of 174 gold-ineligible called eligible; eligible precision 0.973 (144 of 148), recall 1.000; ineligible precision 0.915 (161 of 176) |
| held-out (223 jobs, labels sealed from the builder), independent reviewer, round 2 | scored on the full descriptions; disagreements checked against the full text | eligible precision 0.958, recall 0.958; ineligible precision 0.967 (0.993 adjudicated), recall 0.967; uncertain share 0.211; accuracy 0.946 (0.951 adjudicated); 0 ineligible called eligible; 0 eligible called ineligible; every Ethiopia-located job eligible; 98% of quoted evidence found in the job text; output identical on every rerun |
| roles (320 titles the profile had never seen, labeled blind) | relevance and role family, scored once before any tuning on them | relevance precision 0.934, recall 0.908; role family accuracy 0.887 |

The held-out relevance score is not clean: round 1 printed held-out titles in its report and the
profile was then fixed with phrases from them, so the reviewer used the 400 audit titles (0.955 to
0.963 precision, 0.934 to 0.956 recall, by the reviewer's own labels) and the sealed 320 above
instead. Round 1 of the held-out review, before the fixes, had failed the relevance precision bar
(0.853, a bare "engineer" counted as software) and found 25 phrasings of a closed job that came out
eligible ("the U.S.A." with periods, "all countries except Ethiopia", "US residents only", a list
after a colon, a sentence broken across two lines...). Round 2 found 38 more in a fresh probe of 70
nearby wordings ("must live in", "with the exception of", "Excluded countries:", "domiciled in",
"only employ people in countries where we have an entity"...), a sentence joiner that was
quadratic on a hostile posting, and near-miss time zones read as eligible. Those are fixed after the
review (they were not re-reviewed), each with a row in `TestAdversarial_ClosedJobsAreNeverEligible`.
Other phrasings can exist, and non-English ones are not read at all (a German "Sie müssen in
Deutschland wohnen" with a Worldwide tag comes out eligible).

`TestDevGold_Eligibility` and `TestDevGold_RoleFamilyAndRelevance` re-run the dev scoring and fail
under the rubric's bars (eligible precision 0.90, recall 0.85, ineligible precision 0.95, recall
0.85, uncertain at most 0.25, at most 1 ineligible called eligible, relevance 0.90). `TestPolicies`
has a row for each rubric policy the rules implement (P9, a clinical licence tied to a US state, is
not implemented; P19 conflicts are in `TestAdversarial_*`). Re-run: `go test ./internal/filtering/...`.

What the numbers mean: in the corpus of 21,338 open jobs, 1,028 are in the Ethiopian list (1,027
eligible; the other is a posting in the Ethiopian list whose text says it is in Nairobi), and of the
20,310 worldwide jobs 481 (2.4%) are open to Ethiopia, 5,585 (27%) are unclear and 14,244 (70%) are
not. Of the eligible worldwide ones, 244 say worldwide, 194 name a region that contains Ethiopia
(EMEA), 29 fit by time zone, 11 are explicitly permitted and 3 are "anywhere except".

### Limits, stated plainly

- **The labels are not perfect.** Two labelers disagreed on the boundary between "office city, no
  work mode" and "hybrid" (a `#LI-Hybrid` tag): the dev labeler called it uncertain, the audit
  labeler ineligible. Both saw a snippet (the first 400 characters and up to five sentences with
  location words) while the classifier reads the whole description, so some disagreements are the
  classifier being right on text the labeler never saw ("This role will be hybrid"). Measured
  precision for `ineligible` on the audit set (0.93) counts those against it.
- **Precision is favored over coverage.** A wrong `eligible` wastes an application, a wrong
  `ineligible` hides a job, so the rules answer `uncertain` when the text does not decide it.
  28% of worldwide jobs are `uncertain`, mostly an office city with no work mode and a bare
  "Remote".
- **Ethiopia only.** The classifier takes a target country and UTC offset (`filtering.Classifier`)
  but only Ethiopia is wired and tested.
- **English.** Places are matched in English and a few native spellings; a description in Chinese or
  Hungarian is read for its location field only.
- **Sales territories and clearances** are covered where they name a place ("UKI", "Top Secret
  clearance"). A licence tied to a US state ("licensed to practice in Michigan") is not read yet.
  A US-only job whose restriction is written in a shape the rules do not read, most likely another
  language ("Sie müssen in Deutschland wohnen") or an unusual sentence, and whose location says
  Worldwide or EMEA, comes out `eligible`. That is the costly error, so it is the one the tests and
  the reviewer attack first.
- **Stored verdicts are served until they are recomputed.** After a rules change or a job edit the
  API keeps the old verdict until `aggregator classify` runs (`ingest` runs it), so a job never
  flips to "unclassified" in between.
- **A public "wrong label?" button is not built.** It needs an unauthenticated write endpoint, which
  needs auth or rate limiting first. Until then, corrections become new dev cases by hand.
- **The API default shows every job.** The frozen rubric proposed an "eligible only" default; the
  owner asked for all worldwide jobs to stay visible, so the UI filter defaults to "Any location".

## Changing a rule

Test first: add a row to `TestPolicies` (`internal/filtering/classify_test.go`) that fails, change
the rule, run `go test ./internal/filtering/...` (the dev eval must stay above the bars), and bump
`filtering.Version` when a change can alter a stored verdict so `aggregator classify` recomputes
them.
