-- Priority companies: employers whose jobs the board badges as "Ethiopian
-- company" and can filter to (the curated list in
-- configs/ethiopian_companies.json). A flag on companies, not a table of
-- its own: it is one bit about the employer, and the jobs list already joins
-- companies, so filtering by it costs no extra join. (It does not affect
-- sort order, which is by recency only.)
ALTER TABLE companies ADD COLUMN is_priority BOOLEAN NOT NULL DEFAULT false;
