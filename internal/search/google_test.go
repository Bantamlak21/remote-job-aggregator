package search

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *url.URL) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}

	cfg := httpclient.DefaultConfig()
	cfg.Timeout = 5 * time.Second
	c := New(httpclient.New(cfg), Config{APIKey: "test-key", SearchEngineID: "test-cx"})
	return c, base
}

// withTestEndpoint temporarily points the package-level endpoint at a
// fake server's address for the duration of the test, restoring it
// afterward — the package has one hardcoded constant, not an injectable
// base URL, since nothing in this project ever talks to a different
// Custom Search endpoint. Tests still need to reach a fake server, so
// this is the seam.
func withTestEndpoint(t *testing.T, base *url.URL) {
	t.Helper()
	original := endpoint
	endpoint = base.String() + "/customsearch/v1"
	t.Cleanup(func() { endpoint = original })
}

func TestSearch_ParsesResults(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"items": [
				{"title": "Acme Careers", "link": "https://boards.greenhouse.io/acme", "snippet": "Join Acme"},
				{"title": "Acme on Lever", "link": "https://jobs.lever.co/acme", "snippet": "Careers at Acme"}
			]
		}`))
	})
	withTestEndpoint(t, base)

	results, err := c.Search(context.Background(), `"Acme" site:boards.greenhouse.io`)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Title != "Acme Careers" || results[0].URL != "https://boards.greenhouse.io/acme" || results[0].Snippet != "Join Acme" {
		t.Errorf("results[0] = %+v, unexpected fields", results[0])
	}
}

func TestSearch_NoItemsReturnsEmptySliceNotError(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"searchInformation": {"totalResults": "0"}}`))
	})
	withTestEndpoint(t, base)

	results, err := c.Search(context.Background(), "a query with no matches")
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestSearch_SendsRequiredQueryParams(t *testing.T) {
	var gotQuery url.Values
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items": []}`))
	})
	withTestEndpoint(t, base)

	if _, err := c.Search(context.Background(), `"Acme" site:boards.greenhouse.io`); err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if gotQuery.Get("cx") != "test-cx" {
		t.Errorf("cx = %q, want %q", gotQuery.Get("cx"), "test-cx")
	}
	if gotQuery.Get("q") != `"Acme" site:boards.greenhouse.io` {
		t.Errorf("q = %q, want the exact query string", gotQuery.Get("q"))
	}
	if gotQuery.Get("num") != "10" {
		t.Errorf("num = %q, want %q", gotQuery.Get("num"), "10")
	}
	if gotQuery.Has("key") {
		t.Error(`query string has "key" set — the API key must travel as the X-goog-api-key header, never in the URL (see google.go's comment on why)`)
	}
}

// Regression test for the credential-leak blocker found by adversarial
// review: the API key must be sent as a header, never appear anywhere
// in the request URL — checked here directly against the actual
// request the server receives, not just "the code calls Header.Set".
func TestSearch_APIKeyTravelsAsHeaderNeverInURL(t *testing.T) {
	var gotHeader, gotRawURL string
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-goog-api-key")
		gotRawURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items": []}`))
	})
	withTestEndpoint(t, base)

	if _, err := c.Search(context.Background(), "anything"); err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if gotHeader != "test-key" {
		t.Errorf("X-goog-api-key header = %q, want %q", gotHeader, "test-key")
	}
	if strings.Contains(gotRawURL, "test-key") {
		t.Errorf("request URL = %q, must never contain the API key", gotRawURL)
	}
}

// Regression test proving the specific leak adversarial review found:
// a network error must never expose the API key, even through Go's own
// *url.Error (which embeds the full request URL, query string included,
// in its Error() string — a leak that survives any wrapping done after
// the fact, since the string is already rendered by then). This only
// stays closed because the key is a header, not a query param; if a
// future change moved it back into the URL, this test would catch it.
func TestSearch_NetworkErrorNeverExposesAPIKey(t *testing.T) {
	cfg := httpclient.DefaultConfig()
	cfg.MaxRetries = 0
	cfg.Timeout = 2 * time.Second
	c := New(httpclient.New(cfg), Config{APIKey: "AIzaSyTOPSECRETKEY123456789", SearchEngineID: "test-cx"})

	original := endpoint
	// An address nothing listens on: guarantees a network-level error
	// (connection refused) rather than a real response, which is the
	// exact error shape (*url.Error wrapping a dial error) the leak was
	// found in.
	endpoint = "http://127.0.0.1:1/customsearch/v1"
	t.Cleanup(func() { endpoint = original })

	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected a network error dialing a port nothing listens on, got nil")
	}
	if strings.Contains(err.Error(), "AIzaSyTOPSECRETKEY123456789") {
		t.Errorf("network error leaked the API key: %v", err)
	}
}

func TestSearch_APIErrorIsReported(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": {"code": 400, "message": "API key not valid", "status": "INVALID_ARGUMENT"}}`))
	})
	withTestEndpoint(t, base)

	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error for an invalid API key, got nil")
	}
	if !strings.Contains(err.Error(), "API key not valid") {
		t.Errorf("error = %q, want it to include the API's own message", err.Error())
	}
	if errors.Is(err, ErrQuotaExceeded) {
		t.Error("an invalid-API-key error must not match ErrQuotaExceeded")
	}
}

func TestSearch_QuotaExceededIsDistinguishable(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error": {"code": 429, "message": "Quota exceeded for quota metric", "status": "RESOURCE_EXHAUSTED"}}`))
	})
	withTestEndpoint(t, base)

	cfg := httpclient.DefaultConfig()
	cfg.MaxRetries = 0 // avoid burning test time on httpclient's own 429 backoff retries
	c = New(httpclient.New(cfg), Config{APIKey: "test-key", SearchEngineID: "test-cx"})

	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error for quota exhaustion, got nil")
	}
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("error = %v, want it to match ErrQuotaExceeded", err)
	}
}

// Regression test for a gap found by adversarial review: the classic
// Google API error shape reports a 403 with Status left blank and the
// real signal in errors[0].reason — the original code only checked the
// newer Status=="RESOURCE_EXHAUSTED" convention and missed this one
// entirely (rendered as "API error (): Daily Limit Exceeded", the blank
// parens being the tell). Not yet confirmed against a live key that
// Custom Search specifically returns this shape — see quotaExceeded's
// doc comment — but this is the documented shape for the API family it
// belongs to, and the code must not silently misclassify it if it does
// occur.
func TestSearch_QuotaExceededRecognizesClassicDailyLimitShape(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error": {"code": 403, "message": "Daily Limit Exceeded", "errors": [{"reason": "dailyLimitExceeded"}]}}`))
	})
	withTestEndpoint(t, base)

	cfg := httpclient.DefaultConfig()
	cfg.MaxRetries = 0
	c = New(httpclient.New(cfg), Config{APIKey: "test-key", SearchEngineID: "test-cx"})

	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error for the classic daily-limit shape, got nil")
	}
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("error = %v, want it to match ErrQuotaExceeded (classic reason shape, blank Status)", err)
	}
}

func TestSearch_UnexpectedStatusWithNoParseableErrorBody(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	})
	withTestEndpoint(t, base)

	cfg := httpclient.DefaultConfig()
	cfg.MaxRetries = 0
	c = New(httpclient.New(cfg), Config{APIKey: "test-key", SearchEngineID: "test-cx"})

	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error for a 500 with an unparseable body, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention the status code", err.Error())
	}
}

func TestSearch_RespectsContextCancellation(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{"items": []}`))
	})
	withTestEndpoint(t, base)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.Search(ctx, "anything")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Search() with an already-canceled context = nil error, want one")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Search() did not return promptly for a canceled context")
	}
}
