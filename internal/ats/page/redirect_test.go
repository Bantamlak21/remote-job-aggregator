package page

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
	"github.com/Bantamlak12/remote-job-aggregator/internal/robots"
)

// These tests use the REAL httpclient and the REAL robots.Checker against
// httptest servers, because the property under test (a redirect must not
// launder a request to a robots-disallowed URL) lives in how the three fit
// together, and a fake Doer would never follow a redirect at all.

func realFetcher(t *testing.T) *Fetcher {
	t.Helper()
	cfg := httpclient.DefaultConfig()
	cfg.Timeout = 5 * time.Second
	cfg.UserAgent = "remote-job-aggregator/1.0 (test)"
	hc := httpclient.New(cfg)
	return NewFetcher(hc, robots.New(hc, "remote-job-aggregator"))
}

func TestFetch_RedirectToARobotsDisallowedPathIsNeverRequested(t *testing.T) {
	var secretHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	})
	mux.HandleFunc("/jobs/a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/private/secret", http.StatusFound)
	})
	mux.HandleFunc("/private/secret", func(w http.ResponseWriter, r *http.Request) {
		secretHits.Add(1)
		_, _ = w.Write([]byte("<title>secret</title>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := realFetcher(t).Fetch(context.Background(), srv.URL+"/jobs/a")
	if !errors.Is(err, ErrDisallowed) {
		t.Fatalf("Fetch() error = %v, want ErrDisallowed", err)
	}
	if secretHits.Load() != 0 {
		t.Errorf("the disallowed target was requested %d times; the redirect laundered the fetch", secretHits.Load())
	}
}

func TestFetch_AllowedRedirectOnTheSameSiteIsFollowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	})
	mux.HandleFunc("/jobs/a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jobs/a/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/jobs/a/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<title>Analyst</title>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := realFetcher(t).Fetch(context.Background(), srv.URL+"/jobs/a")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if p.Title != "Analyst" {
		t.Errorf("Title = %q", p.Title)
	}
}

// The guard never touches the network for a cross-host target: it decides
// from the URL alone, so the other host is not even asked for robots.txt.
func TestRedirectGuard_RefusesAnotherHostWithoutAnyNetworkCall(t *testing.T) {
	noNetwork := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) {
		t.Errorf("network call to %s", req.URL)
		return nil, errors.New("unexpected")
	}}
	guard := RedirectGuard(context.Background(), "example.test", robots.New(noNetwork, "remote-job-aggregator"))
	for _, target := range []string{"https://elsewhere.example/landing", "https://example.test.evil.example/x", "https://sub.example.test/x"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if err := guard(req); !errors.Is(err, ErrCrossHostRedirect) {
			t.Errorf("guard(%s) error = %v, want ErrCrossHostRedirect", target, err)
		}
	}
}

func TestRedirectGuard_RefusesAnHTTPSToHTTPDowngrade(t *testing.T) {
	guard := RedirectGuard(context.Background(), "example.test", allowAll)

	prev, _ := http.NewRequest(http.MethodGet, "https://example.test/a", nil)
	down, _ := http.NewRequest(http.MethodGet, "http://example.test/a", nil)
	down.Response = &http.Response{Request: prev}
	if err := guard(down); err == nil {
		t.Error("guard allowed a redirect from https to http")
	}

	up, _ := http.NewRequest(http.MethodGet, "https://example.test/b", nil)
	plainPrev, _ := http.NewRequest(http.MethodGet, "http://example.test/a", nil)
	up.Response = &http.Response{Request: plainPrev}
	if err := guard(up); err != nil {
		t.Errorf("guard refused an http -> https upgrade: %v", err)
	}
}

func TestRedirectGuard_UnreadableRobotsOnTheTargetFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	hc := httpclient.New(func() httpclient.Config {
		c := httpclient.DefaultConfig()
		c.MaxRetries = 0
		c.Timeout = 5 * time.Second
		return c
	}())
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	guard := RedirectGuard(context.Background(), req.URL.Host, robots.New(hc, "remote-job-aggregator"))
	if err := guard(req); !errors.Is(err, ErrDisallowed) {
		t.Errorf("guard error = %v, want ErrDisallowed when the target's robots.txt is unreadable", err)
	}
}
