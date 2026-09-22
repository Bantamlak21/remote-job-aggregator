package search

import (
	"context"
	"encoding/json"
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
	c := New(httpclient.New(cfg), Config{APIKey: "test-key"})
	return c, base
}

// withTestEndpoint temporarily points the package-level endpoint at a
// fake server's address for the duration of the test, restoring it
// afterward — the package has one hardcoded constant, not an injectable
// base URL, since nothing in this project ever talks to a different
// Serper endpoint. Tests still need to reach a fake server, so this is
// the seam.
func withTestEndpoint(t *testing.T, base *url.URL) {
	t.Helper()
	original := endpoint
	endpoint = base.String() + "/search"
	t.Cleanup(func() { endpoint = original })
}

func TestSearch_ParsesResults(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"organic": [
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

func TestSearch_NoOrganicResultsReturnsEmptySliceNotError(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"searchParameters": {"q": "a query with no matches"}}`))
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

func TestSearch_SendsRequiredRequestFields(t *testing.T) {
	var gotMethod, gotContentType string
	var gotBody map[string]any
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"organic": []}`))
	})
	withTestEndpoint(t, base)

	if _, err := c.Search(context.Background(), `"Acme" site:boards.greenhouse.io`); err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody["q"] != `"Acme" site:boards.greenhouse.io` {
		t.Errorf(`body["q"] = %v, want the exact query string`, gotBody["q"])
	}
	if gotBody["num"] != float64(10) {
		t.Errorf(`body["num"] = %v, want 10`, gotBody["num"])
	}
}

// Regression test mirroring the credential-leak concern the Google
// Custom Search client this replaced had to fix: the API key must be
// sent as a header, never appear anywhere in the request URL or body.
func TestSearch_APIKeyTravelsAsHeaderNeverInURLOrBody(t *testing.T) {
	var gotHeader, gotRawURL string
	var gotBody map[string]any
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-API-KEY")
		gotRawURL = r.URL.String()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"organic": []}`))
	})
	withTestEndpoint(t, base)

	if _, err := c.Search(context.Background(), "anything"); err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if gotHeader != "test-key" {
		t.Errorf("X-API-KEY header = %q, want %q", gotHeader, "test-key")
	}
	if strings.Contains(gotRawURL, "test-key") {
		t.Errorf("request URL = %q, must never contain the API key", gotRawURL)
	}
	for k, v := range gotBody {
		if s, ok := v.(string); ok && strings.Contains(s, "test-key") {
			t.Errorf("request body field %q = %q, must never contain the API key", k, s)
		}
	}
}

// Regression test: a network error must never expose the API key, even
// through Go's own *url.Error. This client never puts the key in the
// URL at all (it's a header, and the query travels in the POST body,
// not the URL), so there is structurally nothing for a *url.Error to
// leak here — this test pins that guarantee so a future change can't
// silently reintroduce it.
func TestSearch_NetworkErrorNeverExposesAPIKey(t *testing.T) {
	cfg := httpclient.DefaultConfig()
	cfg.MaxRetries = 0
	cfg.Timeout = 2 * time.Second
	c := New(httpclient.New(cfg), Config{APIKey: "SUPERSECRETSERPERKEY123456789"})

	original := endpoint
	// An address nothing listens on: guarantees a network-level error
	// (connection refused) rather than a real response.
	endpoint = "http://127.0.0.1:1/search"
	t.Cleanup(func() { endpoint = original })

	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected a network error dialing a port nothing listens on, got nil")
	}
	if strings.Contains(err.Error(), "SUPERSECRETSERPERKEY123456789") {
		t.Errorf("network error leaked the API key: %v", err)
	}
}

func TestSearch_MissingQueryParameterErrorIsReported(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Missing query parameter"}`))
	})
	withTestEndpoint(t, base)

	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error for a 400 missing-query-parameter response, got nil")
	}
	if !strings.Contains(err.Error(), "Missing query parameter") {
		t.Errorf("error = %q, want it to include Serper's own message", err.Error())
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Error("a 400 missing-query-parameter error must not match ErrUnauthorized")
	}
}

func TestSearch_UnauthorizedIsDistinguishable(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"Unauthorized.","statusCode":403}`))
			})
			withTestEndpoint(t, base)

			_, err := c.Search(context.Background(), "anything")
			if err == nil {
				t.Fatal("expected an error for an unauthorized response, got nil")
			}
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("error = %v, want it to match ErrUnauthorized", err)
			}
		})
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
	c = New(httpclient.New(cfg), Config{APIKey: "test-key"})

	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error for a 500 with an unparseable body, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention the status code", err.Error())
	}
}

func TestSearch_UnparseableSuccessBodyIsReported(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	})
	withTestEndpoint(t, base)

	_, err := c.Search(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error for an unparseable 200 body, got nil")
	}
}

func TestSearch_RespectsContextCancellation(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{"organic": []}`))
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
