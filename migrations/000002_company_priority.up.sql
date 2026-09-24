-- Priority companies: employers whose jobs the board pins above everything
-- else and badges as "Ethiopian company" (the curated list in
-- configs/ethiopian_companies.json). A flag on companies, not a table of
-- its own: it is one bit about the employer, and the jobs list already joins
-- companies, so ordering and filtering by it costs no extra join.
ALTER TABLE companies ADD COLUMN is_priority BOOLEAN NOT NULL DEFAULT false;
