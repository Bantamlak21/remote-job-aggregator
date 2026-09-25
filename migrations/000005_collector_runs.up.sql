-- When each many-employer collector last ran. Job boards limit how often they
-- may be called (Remotive: at most 4 requests a day; Himalayas refreshes once a
-- day), and ingest runs as separate processes, so the interval has to be
-- remembered somewhere every process can see.
CREATE TABLE collector_runs (
    name        TEXT PRIMARY KEY CHECK (length(btrim(name)) > 0),
    last_run_at TIMESTAMPTZ NOT NULL
);
