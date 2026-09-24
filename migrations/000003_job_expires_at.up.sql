-- Application deadline: sources that publish one (Ethiojobs: date_expiry;
-- careers pages: JSON-LD validThrough) store it here, and the public API
-- stops serving a job once it has passed, instead of showing a posting
-- nobody can still apply to. NULL means "no known deadline".
ALTER TABLE jobs ADD COLUMN expires_at TIMESTAMPTZ;

-- Data step: the per-company search source used to read Ethiojobs too
-- (source_job_id 'ethiojobs:<id>' under provider 'search'). Ethiojobs now
-- has its own source (provider 'ethiojobs'), so those rows would show every
-- such job twice. Close them once; closing is what ingestion would do to
-- rows that are no longer refreshed. The down migration does not reopen them,
-- and nothing writes them again. Run "aggregator ingest" right after
-- migrating: until the direct Ethiojobs source has run, those postings are
-- absent from the board.
UPDATE jobs
SET status = 'removed', closed_at = now()
WHERE source = 'search' AND source_job_id LIKE 'ethiojobs:%' AND status = 'open';
