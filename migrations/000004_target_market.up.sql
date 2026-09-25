-- Which list a job belongs to: 'ethiopia' (jobs in Ethiopia or from Ethiopian
-- companies, shown in the Ethiopian category) or 'worldwide' (companies hiring
-- across borders, shown on the main page). The market is a property of the
-- source a job was collected through (its target), not of the company: one
-- company can have a worldwide Greenhouse board and an Ethiopian Ethiojobs
-- posting at once.
ALTER TABLE target_companies
    ADD COLUMN market TEXT NOT NULL DEFAULT 'worldwide'
        CHECK (market IN ('ethiopia', 'worldwide'));

-- Data step: the market follows the source. Everything registered before this
-- migration that is not an ATS board (feed, careers-site, search, ethiojobs,
-- linkedin) came from the Ethiopian priority list or the Ethiopian sources.
-- ATS boards (Greenhouse, Lever, Ashby) stay worldwide, whichever company
-- owns them.
UPDATE target_companies
SET market = 'ethiopia'
WHERE ats_provider IN ('feed', 'careers-site', 'search', 'ethiojobs', 'linkedin');
