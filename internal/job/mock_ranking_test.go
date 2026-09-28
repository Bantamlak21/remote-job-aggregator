package job

import (
	"context"
	"testing"
	"time"
)

// Same shape and reasoning as ranking_test.go's seedRankingJobs (dates
// deliberately contradict the ranked order), so JOB_REPOSITORY=mock exercises
// the identical ordering a caller would see against real Postgres.
func rankingFixtureJobs() []Job {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	return []Job{
		{ID: "1", Title: "A", PostedAt: day(1), Eligibility: &Eligibility{Status: "eligible"}, Role: &Role{Relevant: true}},
		{ID: "2", Title: "B", PostedAt: day(30), Eligibility: &Eligibility{Status: "eligible"}, Role: &Role{Relevant: false}},
		{ID: "3", Title: "C", PostedAt: day(15), IsPriority: true}, // no Eligibility/Role at all: unclassified
		{ID: "4", Title: "D", PostedAt: day(20), Eligibility: &Eligibility{Status: "uncertain"}, Role: &Role{Relevant: false}},
		{ID: "5", Title: "E", PostedAt: day(29), IsPriority: true, Eligibility: &Eligibility{Status: "ineligible"}, Role: &Role{Relevant: false}},
	}
}

func TestMockRepository_SortRankedOrdersByEligibilityThenRelevanceThenPriorityThenRecency(t *testing.T) {
	repo := &MockRepository{jobs: rankingFixtureJobs()}

	res, err := repo.List(context.Background(), Filter{Sort: SortRanked})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	want := []string{"1", "2", "3", "4", "5"}
	if got := idsOf(res.Jobs); !equalStrings(got, want) {
		t.Errorf("ranked order = %v, want %v", got, want)
	}
}

func TestMockRepository_DefaultSortIsStillRecencyRegardlessOfEligibility(t *testing.T) {
	repo := &MockRepository{jobs: rankingFixtureJobs()}

	res, err := repo.List(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	want := []string{"2", "5", "4", "3", "1"} // newest (day 30) to oldest (day 1)
	if got := idsOf(res.Jobs); !equalStrings(got, want) {
		t.Errorf("default recency order = %v, want %v", got, want)
	}
}
