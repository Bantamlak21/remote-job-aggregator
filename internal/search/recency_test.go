package search

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestSearchRecent_SendsTimeFilterAndReturnsDates(t *testing.T) {
	var got map[string]any
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"organic":[
			{"title":"A","link":"https://ethiojobs.net/job/x","snippet":"s","date":"19 hours ago"},
			{"title":"B","link":"https://ethiojobs.net/job/y","snippet":"s"}]}`))
	})
	withTestEndpoint(t, base)

	results, err := c.SearchRecent(context.Background(), "q", RecencyMonth)
	if err != nil {
		t.Fatalf("SearchRecent() failed: %v", err)
	}
	if got["tbs"] != "qdr:m" {
		t.Errorf("request tbs = %v, want qdr:m", got["tbs"])
	}
	if len(results) != 2 || results[0].Date != "19 hours ago" || results[1].Date != "" {
		t.Errorf("results = %+v, want dates carried through (and empty when Serper gives none)", results)
	}
}

func TestSearch_PlainSearchSendsNoTimeFilter(t *testing.T) {
	var got map[string]any
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{}`))
	})
	withTestEndpoint(t, base)

	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if _, present := got["tbs"]; present {
		t.Errorf("plain Search sent tbs=%v; discovery's searches must stay unrestricted", got["tbs"])
	}
}

// The budget counts calls, so a call must be one wire request even when the
// server keeps failing (the shared client would otherwise retry 3 times).
func TestSearchRecent_OneCallIsOneWireRequestEvenOnServerErrors(t *testing.T) {
	var wire atomic.Int64
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		wire.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	withTestEndpoint(t, base)

	if _, err := c.SearchRecent(context.Background(), "q", RecencyMonth); err == nil {
		t.Fatal("SearchRecent() succeeded against a failing server")
	}
	if got := wire.Load(); got != 1 {
		t.Errorf("server saw %d requests for one SearchRecent call, want 1", got)
	}
}

// Discovery's plain Search keeps the shared client's retries.
func TestSearch_PlainSearchStillRetriesTransientFailures(t *testing.T) {
	var wire atomic.Int64
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if wire.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"organic":[]}`))
	})
	withTestEndpoint(t, base)

	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if wire.Load() != 2 {
		t.Errorf("wire requests = %d, want 2 (one retry)", wire.Load())
	}
}

func TestSearchRecentPage_SendsThePageAndKeepsTheFilterAndOneWireRequest(t *testing.T) {
	var bodies []map[string]any
	var wire atomic.Int64
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		wire.Add(1)
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.WriteHeader(http.StatusInternalServerError) // also proves no transparent retry on pages
	})
	withTestEndpoint(t, base)

	for _, page := range []int{1, 3} {
		if _, err := c.SearchRecentPage(context.Background(), "q", RecencyDay, page); err == nil {
			t.Fatalf("page %d: succeeded against a failing server", page)
		}
	}
	if wire.Load() != 2 {
		t.Fatalf("wire requests = %d, want 2 (one per call)", wire.Load())
	}
	if _, has := bodies[0]["page"]; has {
		t.Errorf("page 1 sent page=%v; the default page is omitted", bodies[0]["page"])
	}
	if bodies[1]["page"] != float64(3) || bodies[1]["tbs"] != "qdr:d" {
		t.Errorf("page 3 body = %v, want page=3 and tbs=qdr:d", bodies[1])
	}
}

func TestSearchRecentPage_RejectsOutOfRangePagesWithoutANetworkCall(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request made for an invalid page")
	})
	withTestEndpoint(t, base)
	for _, page := range []int{0, -1, maxPage + 1, 1 << 30} {
		if _, err := c.SearchRecentPage(context.Background(), "q", RecencyDay, page); err == nil {
			t.Errorf("page %d accepted", page)
		}
	}
}

func TestSearchRecent_RejectsUnknownWindowWithoutANetworkCall(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request made for an invalid recency")
	})
	withTestEndpoint(t, base)

	for _, bad := range []Recency{"", "x", "qdr:m", "m,y"} {
		if _, err := c.SearchRecent(context.Background(), "q", bad); err == nil {
			t.Errorf("SearchRecent(recency=%q) succeeded, want an error", bad)
		}
	}
}
