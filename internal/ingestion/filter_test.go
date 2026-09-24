package ingestion

import (
	"context"
	"errors"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
)

func TestOnlyProviders_KeepsOnlyTheNamedProviders(t *testing.T) {
	all := []company.TargetCompany{
		testTarget(1, 1, "greenhouse", "a"),
		testTarget(2, 2, "search", "B"),
		testTarget(3, 3, "feed", "https://c/feed"),
		testTarget(4, 4, "search", "D"),
	}
	got, err := OnlyProviders(&fakeTargetLister{targets: all}, "search", "feed").ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive() error = %v", err)
	}
	if len(got) != 3 || got[0].ID != 2 || got[1].ID != 3 || got[2].ID != 4 {
		t.Errorf("got %+v, want targets 2, 3, 4 in their original order", got)
	}
}

func TestOnlyProviders_NoProvidersListsNothing(t *testing.T) {
	got, err := OnlyProviders(&fakeTargetLister{targets: []company.TargetCompany{testTarget(1, 1, "greenhouse", "a")}}).ListActive(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want an empty list (an empty allow-list must not mean 'everything')", got, err)
	}
}

func TestOnlyProviders_PropagatesListErrors(t *testing.T) {
	boom := errors.New("db down")
	if _, err := OnlyProviders(&fakeTargetLister{err: boom}, "feed").ListActive(context.Background()); !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the inner error", err)
	}
}

func TestOnlyProviders_FeedsTheIngesterOnlyTheAllowedTargets(t *testing.T) {
	jobs := &fakeJobUpserter{}
	client := &fakeATSClient{}
	in := New(
		OnlyProviders(&fakeTargetLister{targets: []company.TargetCompany{
			testTarget(1, 1, "greenhouse", "a"), testTarget(2, 2, "feed", "https://b/feed"),
		}}, "feed"),
		&fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"greenhouse": client, "feed": client}, 1, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(results) != 1 || results[0].Target.ID != 2 || len(client.calls) != 1 || client.calls[0] != "https://b/feed" {
		t.Errorf("results = %+v, calls = %v; want only the feed target fetched", results, client.calls)
	}
}
