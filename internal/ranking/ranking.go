// Package ranking is the deterministic, explainable ordering behind the public
// job list's optional "sort=ranked" mode (see docs/api.md). It computes nothing
// new and stores nothing: eligibility, role relevance, and priority-company
// status already come out of Phase 3 (ingestion) and Phase 4 (classification),
// so ranking is purely a sort key over data that already exists. There is
// deliberately no ranking table, no ranking CLI command, and no ingestion-time
// step — a stale score that needed re-computation would be a bug class this
// design has no room for, because there is no score to go stale.
//
// The order, highest first (see docs/api.md and PROJECT_STATUS.md's Phase 5
// notes for the reasoning): a job's eligibility tier, then whether its role is
// relevant, then whether it is from a priority (Ethiopian tech) company, then
// how recently it was posted. Newest-first (no ranking) stays the list's
// default — Bantamlak asked for every job to stay visible and ordered by
// recency by default (the same reasoning Phase 4 applied to the location
// filter); "ranked" is an explicit opt-in, never the default.
package ranking

import "fmt"

// Tier orders an eligibility verdict for ranking. Numerically higher ranks
// first. "Eligible" ranks highest; "ineligible" ranks lowest, because a job we
// know is closed to Ethiopia is worse to see than one we simply don't have a
// verdict for yet. "Uncertain" (the classifier ran and could not tell) and
// "unclassified" (the classifier has not run yet, or the job predates Phase 4)
// are treated alike: both mean "we don't know", and neither should outrank or
// underrank the other.
type Tier int

const (
	TierIneligible Tier = 0
	TierUnknown    Tier = 1
	TierEligible   Tier = 2
)

// EligibilityTier maps an eligibility status ("eligible", "ineligible",
// "uncertain", "unclassified", or "" for a job with no job_eligibility row at
// all) to its ranking tier. An unrecognized status is treated as unknown
// rather than erroring: ranking must never fail a request over a data
// inconsistency the eligibility filter itself already tolerates the same way
// (see job.ValidEligibilityFilter's "unclassified").
func EligibilityTier(status string) Tier {
	switch status {
	case "eligible":
		return TierEligible
	case "ineligible":
		return TierIneligible
	default: // "uncertain", "unclassified", "", or anything unrecognized
		return TierUnknown
	}
}

// OrderBySQL is the ORDER BY clause for "sort=ranked". Its tier numbers are
// generated from the same Tier constants EligibilityTier returns, so the two
// cannot drift to different *numbers*; TestOrderBySQLTiersMatchEligibilityTier
// guards against the numbers being assigned to the wrong *status* instead.
// Column names match internal/job/store.go's query: "e" is the job_eligibility
// LEFT JOIN (its status is SQL NULL for an unclassified job, hence ELSE), "c"
// is companies, "posted_at" is the COALESCE(published_at, first_seen_at) alias
// jobColumnsForAPI defines.
var OrderBySQL = fmt.Sprintf(`CASE e.status
		WHEN 'eligible' THEN %d
		WHEN 'ineligible' THEN %d
		ELSE %d
	END DESC,
	COALESCE(e.relevant, false) DESC,
	c.is_priority DESC,
	posted_at DESC,
	j.id DESC`, TierEligible, TierIneligible, TierUnknown)
