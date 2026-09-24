package job

import (
	"context"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

// The fixtures are worldwide remote roles: the worldwide list has all of
// them, the Ethiopian list none, and every job reports its market.
func TestMockRepository_FixturesAreWorldwide(t *testing.T) {
	repo := NewMockRepository()
	ctx := context.Background()

	all, err := repo.List(ctx, Filter{PageSize: MaxPageSize})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	for _, j := range all.Jobs {
		if j.Market != market.Worldwide {
			t.Errorf("fixture %s market = %q, want worldwide", j.ID, j.Market)
		}
	}
	world, _ := repo.List(ctx, Filter{Market: market.Worldwide, PageSize: MaxPageSize})
	eth, _ := repo.List(ctx, Filter{Market: market.Ethiopia, PageSize: MaxPageSize})
	if world.Total != all.Total || eth.Total != 0 {
		t.Errorf("worldwide total %d (want %d), ethiopia total %d (want 0)", world.Total, all.Total, eth.Total)
	}
}
