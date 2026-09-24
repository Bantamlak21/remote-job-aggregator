package httpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWithoutRetries_OneCallIsOneWireRequest(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRetries = 3
	resp, err := doWithin(t, 5*time.Second, New(cfg), newRequest(t, WithoutRetries(context.Background()), srv.URL))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	resp.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Errorf("server saw %d requests with retries disabled, want exactly 1", got)
	}
}

func TestWithoutRetries_AlsoAppliesToNetworkErrors(t *testing.T) {
	var dials atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		// Kill the connection mid-response: a network-level error.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("no hijacker")
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRetries = 3
	_, err := doWithin(t, 5*time.Second, New(cfg), newRequest(t, WithoutRetries(context.Background()), srv.URL))
	if err == nil {
		t.Fatal("Do() succeeded against a server that drops the connection")
	}
	if got := dials.Load(); got != 1 {
		t.Errorf("server saw %d requests, want 1", got)
	}
}

func TestRedirectCheck_RejectionStopsBeforeTheTargetIsContactedAndIsNotRetried(t *testing.T) {
	var targetHits, originHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		http.Redirect(w, r, "/private/secret", http.StatusFound)
	})
	mux.HandleFunc("/private/secret", func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	boom := errors.New("robots says no")
	var seen string
	ctx := WithRedirectCheck(context.Background(), func(next *http.Request) error {
		seen = next.URL.Path
		return boom
	})
	cfg := testConfig()
	cfg.MaxRetries = 3
	_, err := doWithin(t, 5*time.Second, New(cfg), newRequest(t, ctx, srv.URL+"/start"))

	if !errors.Is(err, ErrRedirectRejected) || !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap ErrRedirectRejected and the check's own error", err)
	}
	if seen != "/private/secret" {
		t.Errorf("check saw %q, want the redirect target path", seen)
	}
	if targetHits.Load() != 0 {
		t.Errorf("the redirect target was contacted %d times despite the rejection", targetHits.Load())
	}
	if originHits.Load() != 1 {
		t.Errorf("origin hit %d times, want 1 (a rejected redirect is a settled answer, not a transient error)", originHits.Load())
	}
}

func TestRedirectCheck_AcceptedRedirectIsFollowedAndEveryHopIsChecked(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/c", http.StatusFound) })
	mux.HandleFunc("/c", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("done")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var hops []string
	ctx := WithRedirectCheck(context.Background(), func(next *http.Request) error {
		hops = append(hops, next.URL.Path)
		return nil
	})
	resp, err := doWithin(t, 5*time.Second, New(testConfig()), newRequest(t, ctx, srv.URL+"/a"))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	resp.Body.Close()
	if len(hops) != 2 || hops[0] != "/b" || hops[1] != "/c" {
		t.Errorf("checked hops = %v, want [/b /c]", hops)
	}
}

func TestRedirectCheck_AbsentMeansNoCheck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := doWithin(t, 5*time.Second, New(testConfig()), newRequest(t, context.Background(), srv.URL+"/a"))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
