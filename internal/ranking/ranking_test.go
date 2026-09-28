package ranking

import (
	"regexp"
	"strconv"
	"testing"
)

func TestEligibilityTier(t *testing.T) {
	cases := map[string]Tier{
		"eligible":       TierEligible,
		"ineligible":     TierIneligible,
		"uncertain":      TierUnknown,
		"unclassified":   TierUnknown,
		"":               TierUnknown, // no job_eligibility row at all
		"something-else": TierUnknown, // never errors on an unrecognized value
	}
	for status, want := range cases {
		if got := EligibilityTier(status); got != want {
			t.Errorf("EligibilityTier(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestTierOrdering(t *testing.T) {
	if !(TierEligible > TierUnknown && TierUnknown > TierIneligible) {
		t.Fatalf("tiers are not strictly ordered eligible > unknown > ineligible: %d, %d, %d",
			TierEligible, TierUnknown, TierIneligible)
	}
}

// The SQL's CASE numbers must be exactly the Tier constants' values, not
// independently chosen numbers that happen to match today.
func TestOrderBySQLUsesTheTierConstants(t *testing.T) {
	re := regexp.MustCompile(`WHEN 'eligible' THEN (\d+)\s+WHEN 'ineligible' THEN (\d+)\s+ELSE (\d+)`)
	m := re.FindStringSubmatch(OrderBySQL)
	if m == nil {
		t.Fatalf("OrderBySQL does not match the expected CASE shape:\n%s", OrderBySQL)
	}
	got := [3]string{m[1], m[2], m[3]}
	want := [3]string{strconv.Itoa(int(TierEligible)), strconv.Itoa(int(TierIneligible)), strconv.Itoa(int(TierUnknown))}
	if got != want {
		t.Errorf("OrderBySQL's CASE numbers = %v, want %v (the Tier constants)", got, want)
	}
}

// The CASE's status->number assignment must match EligibilityTier's for every
// status the eligibility classifier (or a caller of the filter) can produce.
// Parses OrderBySQL itself (rather than restating its mapping in Go, which
// would just compare EligibilityTier to a second copy of itself) so a future
// edit to either the SQL or the Go function that breaks their agreement fails
// this test.
func TestOrderBySQLTiersMatchEligibilityTier(t *testing.T) {
	whenClause := regexp.MustCompile(`WHEN '(\w+)' THEN (\d+)`)
	for _, m := range whenClause.FindAllStringSubmatch(OrderBySQL, -1) {
		status, sqlTier := m[1], m[2]
		if got := strconv.Itoa(int(EligibilityTier(status))); got != sqlTier {
			t.Errorf("SQL assigns %q tier %s, but EligibilityTier(%q) = %s", status, sqlTier, status, got)
		}
	}
	elseClause := regexp.MustCompile(`ELSE (\d+)`)
	elseTier := elseClause.FindStringSubmatch(OrderBySQL)[1]
	for _, status := range []string{"uncertain", "unclassified", "", "something-else"} {
		if got := strconv.Itoa(int(EligibilityTier(status))); got != elseTier {
			t.Errorf("SQL's ELSE assigns tier %s, but EligibilityTier(%q) = %s", elseTier, status, got)
		}
	}
}
