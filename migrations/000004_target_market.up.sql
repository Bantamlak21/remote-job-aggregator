-- Which list a job belongs to: 'ethiopia' (jobs in Ethiopia or from Ethiopian
-- companies, shown in the Ethiopian category) or 'worldwide' (companies hiring
-- across borders, shown on the main page). The market is a property of the
-- source a job was collected through (its target), not of the company: one
-- company can have a worldwide Greenhouse board and an Ethiopian Ethiojobs
-- posting at once.
ALTER TABLE target_companies
    ADD COLUMN market TEXT NOT NULL DEFAULT 'worldwide'
        CHECK (market IN ('ethiopia', 'worldwide'));

-- Data step: everything registered before this migration that is not an ATS
-- board came from the Ethiopian priority list or the Ethiopian sources, and
-- so did any target of a priority company. Greenhouse boards of other
-- companies stay worldwide.
UPDATE target_companies t
SET market = 'ethiopia'
WHERE t.ats_provider IN ('feed', 'careers-site', 'search', 'ethiojobs', 'linkedin')
   OR EXISTS (SELECT 1 FROM companies c WHERE c.id = t.company_id AND c.is_priority);
