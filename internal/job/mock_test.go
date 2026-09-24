package job

import (
	"context"
	"testing"
)

func TestMockRepository_ListWithNoFilterReturnsEveryJobNewestFirst(t *testing.T) {
	repo := NewMockRepository()

	result, err := repo.List(context.Background(), Filter{PageSize: 100})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if result.Total != 12 {
		t.Fatalf("Total = %d, want 12 fixture jobs", result.Total)
	}
	if len(result.Jobs) != 12 {
		t.Fatalf("len(Jobs) = %d, want 12", len(result.Jobs))
	}
	for i := 1; i < len(result.Jobs); i++ {
		if result.Jobs[i-1].PostedAt.Before(result.Jobs[i].PostedAt) {
			t.Fatalf("Jobs[%d] (%s) is posted before Jobs[%d] (%s), want newest-first order",
				i-1, result.Jobs[i-1].PostedAt, i, result.Jobs[i].PostedAt)
		}
	}
}

func TestMockRepository_ListFiltersByRemoteType(t *testing.T) {
	repo := NewMockRepository()

	result, err := repo.List(context.Background(), Filter{RemoteType: RemoteTypeHybrid, PageSize: 100})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(result.Jobs) == 0 {
		t.Fatal("expected at least one hybrid job in the fixtures")
	}
	for _, j := range result.Jobs {
		if j.RemoteType != RemoteTypeHybrid {
			t.Errorf("job %s has RemoteType %q, want %q", j.ID, j.RemoteType, RemoteTypeHybrid)
		}
	}
}

func TestMockRepository_ListFiltersByEmploymentType(t *testing.T) {
	repo := NewMockRepository()

	result, err := repo.List(context.Background(), Filter{EmploymentType: EmploymentTypeInternship, PageSize: 100})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(result.Jobs) == 0 {
		t.Fatal("expected at least one internship in the fixtures")
	}
	for _, j := range result.Jobs {
		if j.EmploymentType != EmploymentTypeInternship {
			t.Errorf("job %s has EmploymentType %q, want %q", j.ID, j.EmploymentType, EmploymentTypeInternship)
		}
	}
}

func TestMockRepository_ListFiltersByCompanyCaseInsensitive(t *testing.T) {
	repo := NewMockRepository()

	result, err := repo.List(context.Background(), Filter{Company: "spotify", PageSize: 100})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(result.Jobs) != 1 || result.Jobs[0].CompanyName != "Spotify" {
		t.Fatalf("Jobs = %+v, want exactly one Spotify job", result.Jobs)
	}
}

func TestMockRepository_ListFiltersByTagCaseInsensitive(t *testing.T) {
	repo := NewMockRepository()

	result, err := repo.List(context.Background(), Filter{Tag: "GO", PageSize: 100})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(result.Jobs) == 0 {
		t.Fatal("expected at least one job tagged \"go\"")
	}
	for _, j := range result.Jobs {
		if !hasTagFold(j.Tags, "go") {
			t.Errorf("job %s tags = %v, want one matching \"go\"", j.ID, j.Tags)
		}
	}
}

func TestMockRepository_ListQueryMatchesTitleCompanyOrTags(t *testing.T) {
	repo := NewMockRepository()

	cases := []struct {
		query   string
		wantJob string
	}{
		{"payments", "job_spotify_backend_eng"},      // matches title
		{"DuckDuckGo", "job_duckduckgo_backend_eng"}, // matches company, case-insensitive
		{"kubernetes", "job_gitlab_sre"},             // matches a tag
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			result, err := repo.List(context.Background(), Filter{Query: tc.query, PageSize: 100})
			if err != nil {
				t.Fatalf("List() failed: %v", err)
			}
			found := false
			for _, j := range result.Jobs {
				if j.ID == tc.wantJob {
					found = true
				}
			}
			if !found {
				t.Errorf("query %q: job %s not found in %d results", tc.query, tc.wantJob, len(result.Jobs))
			}
		})
	}
}

func TestMockRepository_ListQueryWithNoMatchesReturnsEmptyNotError(t *testing.T) {
	repo := NewMockRepository()

	result, err := repo.List(context.Background(), Filter{Query: "no such job anywhere", PageSize: 100})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(result.Jobs) != 0 || result.Total != 0 {
		t.Errorf("result = %+v, want zero matches", result)
	}
}

func TestMockRepository_ListPaginates(t *testing.T) {
	repo := NewMockRepository()

	page1, err := repo.List(context.Background(), Filter{Page: 1, PageSize: 5})
	if err != nil {
		t.Fatalf("List() page 1 failed: %v", err)
	}
	if len(page1.Jobs) != 5 {
		t.Fatalf("page 1 len = %d, want 5", len(page1.Jobs))
	}
	if page1.Total != 12 {
		t.Fatalf("page 1 Total = %d, want 12 (total ignores pagination)", page1.Total)
	}

	page3, err := repo.List(context.Background(), Filter{Page: 3, PageSize: 5})
	if err != nil {
		t.Fatalf("List() page 3 failed: %v", err)
	}
	if len(page3.Jobs) != 2 {
		t.Fatalf("page 3 (jobs 11-12 of 12) len = %d, want 2", len(page3.Jobs))
	}

	page4, err := repo.List(context.Background(), Filter{Page: 4, PageSize: 5})
	if err != nil {
		t.Fatalf("List() page 4 failed: %v", err)
	}
	if len(page4.Jobs) != 0 {
		t.Fatalf("page 4 (past the end) len = %d, want 0, not an error or a wrapped-around result", len(page4.Jobs))
	}
}

func TestMockRepository_ListDefaultsPageAndPageSizeWhenUnset(t *testing.T) {
	repo := NewMockRepository()

	result, err := repo.List(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(result.Jobs) != 12 {
		t.Fatalf("len(Jobs) = %d, want all 12 (default page size 20 > 12 fixtures)", len(result.Jobs))
	}
}

func TestMockRepository_ListRespectsContextCancellation(t *testing.T) {
	repo := NewMockRepository()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.List(ctx, Filter{})
	if err == nil {
		t.Error("List() with an already-canceled context = nil error, want one")
	}
}

func TestMockRepository_GetReturnsMatchingJob(t *testing.T) {
	repo := NewMockRepository()

	got, err := repo.Get(context.Background(), "job_spotify_backend_eng")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if got.CompanyName != "Spotify" {
		t.Errorf("CompanyName = %q, want %q", got.CompanyName, "Spotify")
	}
	if got.Description == "" {
		t.Error("Description is empty, want the detail-only field populated")
	}
}

func TestMockRepository_GetUnknownIDReturnsErrNotFound(t *testing.T) {
	repo := NewMockRepository()

	_, err := repo.Get(context.Background(), "does-not-exist")
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMockRepository_GetRespectsContextCancellation(t *testing.T) {
	repo := NewMockRepository()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.Get(ctx, "job_spotify_backend_eng")
	if err == nil {
		t.Error("Get() with an already-canceled context = nil error, want one")
	}
}

func TestRemoteType_Valid(t *testing.T) {
	valid := []RemoteType{RemoteTypeRemote, RemoteTypeHybrid, RemoteTypeOnsite, RemoteTypeUnknown}
	for _, v := range valid {
		if !v.Valid() {
			t.Errorf("%q.Valid() = false, want true", v)
		}
	}
	invalid := []RemoteType{"", "bogus", "FULLY_REMOTE"}
	for _, v := range invalid {
		if v.Valid() {
			t.Errorf("%q.Valid() = true, want false", v)
		}
	}
}

func TestEmploymentType_Valid(t *testing.T) {
	valid := []EmploymentType{EmploymentTypeFullTime, EmploymentTypePartTime, EmploymentTypeContract, EmploymentTypeInternship, EmploymentTypeUnknown}
	for _, v := range valid {
		if !v.Valid() {
			t.Errorf("%q.Valid() = false, want true", v)
		}
	}
	invalid := []EmploymentType{"", "bogus"}
	for _, v := range invalid {
		if v.Valid() {
			t.Errorf("%q.Valid() = true, want false", v)
		}
	}
}
