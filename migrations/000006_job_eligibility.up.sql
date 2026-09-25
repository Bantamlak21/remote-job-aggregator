-- What the filtering package decided about each job: whether a candidate in the
-- target country could take it (geographic eligibility) and what kind of role
-- it is (relevance). One row per job, recomputed when the rules
-- (classifier_version), the target country or the job's content change.
--
-- The verdict is stored, not computed per request: the API filters and paginates
-- on it, and the rules are too slow to run over every job on every list call.
-- A job with no row has not been classified yet (the API reports that as null).
CREATE TABLE job_eligibility (
    job_id             BIGINT PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
    target_country     CHAR(2) NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('eligible', 'ineligible', 'uncertain')),
    confidence         NUMERIC(3, 2) NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    -- The kind of thing the verdict rests on (local_ethiopia, worldwide, ...).
    basis              TEXT NOT NULL CHECK (length(btrim(basis)) > 0),
    reasons            JSONB NOT NULL DEFAULT '[]',
    evidence           JSONB NOT NULL DEFAULT '[]',
    detected_locations JSONB NOT NULL DEFAULT '[]',
    restrictions       JSONB NOT NULL DEFAULT '[]',
    -- Set for an eligible job that asks for working hours far from the
    -- target's ("works US hours"); NULL otherwise.
    hours_constraint   TEXT,
    role_family        TEXT NOT NULL CHECK (length(btrim(role_family)) > 0),
    relevant           BOOLEAN NOT NULL,
    relevance_matched  TEXT,
    -- Which rules and which version of the job this row was computed from; a
    -- row whose version, target or job_content_hash no longer match is stale.
    classifier_version INTEGER NOT NULL,
    job_content_hash   TEXT NOT NULL,
    classified_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX job_eligibility_status_idx ON job_eligibility (status);
CREATE INDEX job_eligibility_relevant_idx ON job_eligibility (relevant) WHERE relevant;
CREATE INDEX job_eligibility_role_family_idx ON job_eligibility (role_family);
