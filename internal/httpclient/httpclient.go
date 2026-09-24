// Package httpclient owns the single outbound HTTP client every
// network-calling package in this project shares. It knows nothing about
// job boards, ATS providers or discovery — it is HTTP infrastructure:
// one pooled, explicitly sized transport, a bounded response body, a
// capped redirect chain, and retry with backoff for the two status
// classes worth retrying (429 and 5xx).
//
// Construct one Client per process (or per distinct set of tuning
// parameters) and share it. Creating a Client per request would defeat
// connection pooling entirely, since each new Client brings its own
// idle-connection pool that is discarded with it.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrResponseTooLarge is returned by a read of a response body that grew
// past Config.MaxResponseBytes. It is returned instead of a silent
// truncation so a caller can tell "this is the whole document" apart from
// "this is the first N bytes of a document" — an important distinction
// when the bytes are then parsed as JSON or HTML, where a truncated
// prefix usually fails in some other, more confusing way.
//
// It carries the package prefix in its own text because it surfaces bare,
// out of a Read call far from any Do, with nothing else to attribute it.
var ErrResponseTooLarge = errors.New("httpclient: response body exceeds the configured maximum size")

// ErrTooManyRedirects is returned when a redirect chain reaches
// Config.MaxRedirects hops. It travels back to the caller wrapped in the
// *url.Error net/http builds, so test it with errors.Is rather than ==.
// Unlike ErrResponseTooLarge it carries no prefix of its own: it only
// ever reaches a caller through Do, which adds one.
var ErrTooManyRedirects = errors.New("too many redirects")

// ErrRedirectRejected wraps whatever error a RedirectCheck returned. It is
// a settled answer, never retried.
var ErrRedirectRejected = errors.New("httpclient: redirect rejected")

type ctxKey int

const (
	redirectCheckKey ctxKey = iota
	noRetryKey
)

// RedirectCheck vets the request a redirect is about to make (its URL is
// the redirect target). Returning an error aborts the request before the
// target is contacted.
type RedirectCheck func(next *http.Request) error

// WithRedirectCheck returns a context that makes every request sent with it
// call check before following each redirect. It exists so callers that
// gate fetches on something host- or path-specific (robots.txt) can apply
// the same gate to where a redirect leads, not only to the URL they asked
// for.
func WithRedirectCheck(ctx context.Context, check RedirectCheck) context.Context {
	return context.WithValue(ctx, redirectCheckKey, check)
}

// WithoutRetries returns a context that makes Do attempt a request exactly
// once. For callers that meter their own requests (a paid API with a query
// budget) and so must know one call is one wire request.
func WithoutRetries(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRetryKey, true)
}

// Backoff shape. Base and cap are deliberately package constants rather
// than Config fields: they are a property of "how to be a polite HTTP
// client", not something a caller should have to decide, and every value
// a caller might reasonably want to change (how many retries, how long a
// request may take overall) is already in Config.
const (
	backoffBase = 200 * time.Millisecond
	backoffCap  = 10 * time.Second
)

// Transport timeouts that are not caller-tunable. These mirror
// http.DefaultTransport's values; they bound individual phases of a
// request, while Config.Timeout bounds the round trip as a whole.
const (
	dialTimeout           = 30 * time.Second
	dialKeepAlive         = 30 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// drainLimit bounds how much of a to-be-retried response body is read
// before closing it. Reading a small body to completion lets the
// transport put the connection back in the idle pool instead of tearing
// it down; reading an arbitrarily large one would defeat the point of
// MaxResponseBytes on the retry path.
const drainLimit = 64 << 10

// Config is the full tuning surface of a Client. Every field is applied
// verbatim — nothing here is silently replaced with a default, so a
// zero-value Config produces a client with no timeout and no retries
// rather than a quietly different client than the one asked for. Start
// from DefaultConfig and change what you need.
//
// Non-positive values have meanings worth knowing:
//   - Timeout <= 0 means no whole-request deadline (net/http's own rule).
//   - UserAgent == "" sends no User-Agent header at all (net/http omits a
//     header that is present but empty) rather than falling back to Go's
//     default "Go-http-client/1.1", which would identify this project as
//     an anonymous bot to every server it talks to.
//   - MaxResponseBytes <= 0 disables the response size limit.
//   - MaxRetries <= 0 means a single attempt and no retries.
//   - MaxRedirects <= 0 refuses to follow any redirect at all.
type Config struct {
	Timeout             time.Duration
	MaxResponseBytes    int64
	UserAgent           string
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	MaxRetries          int
	MaxRedirects        int
}

// DefaultConfig returns the tuning this project runs with: a 10s whole-
// request budget, a 5 MiB response ceiling (far above any ATS JSON page
// or careers HTML page, far below anything that would threaten process
// memory), and a pool sized for talking to many hosts a few connections
// at a time, which is the shape of both discovery and ingestion traffic.
func DefaultConfig() Config {
	return Config{
		Timeout:             10 * time.Second,
		MaxResponseBytes:    5 * 1024 * 1024,
		UserAgent:           "remote-job-aggregator/1.0",
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		MaxRetries:          3,
		MaxRedirects:        5,
	}
}

// Client is a configured, reusable HTTP client. It is safe for concurrent
// use by multiple goroutines, and is meant to be: the idle-connection
// pool it holds only pays off when it is shared.
type Client struct {
	httpClient *http.Client
	cfg        Config
}

// New builds a Client with its own explicitly configured transport.
//
// The transport is constructed here rather than derived from
// http.DefaultTransport because pool sizing must be a deliberate choice:
// DefaultTransport's MaxIdleConnsPerHost is 2, which silently throttles
// any workload that talks to one host repeatedly, and sharing
// DefaultTransport would mean this package's tuning leaked into every
// other user of net/http in the process.
//
// TLS configuration is left untouched, so certificate verification stays
// on with the platform root pool.
func New(cfg Config) *Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: dialKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
	}

	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// via holds every request already made, including the
				// original one — so at the check for the k-th redirect,
				// len(via) == k. ">" (not ">=") is what makes
				// MaxRedirects=N actually permit N redirects, matching
				// its own doc comment: MaxRedirects=0 rejects at k=1
				// (0 redirects allowed), MaxRedirects=1 allows k=1 but
				// rejects at k=2 (exactly 1 redirect allowed). Found by
				// adversarial review: the original ">=" silently
				// permitted only N-1 redirects, so MaxRedirects=1 refused
				// every redirect exactly like MaxRedirects=0.
				if len(via) > cfg.MaxRedirects {
					return fmt.Errorf("%w (limit %d)", ErrTooManyRedirects, cfg.MaxRedirects)
				}
				if check, ok := req.Context().Value(redirectCheckKey).(RedirectCheck); ok {
					if err := check(req); err != nil {
						return fmt.Errorf("%w: %w", ErrRedirectRejected, err)
					}
				}
				return nil
			},
		},
		cfg: cfg,
	}
}

// Do sends req, retrying transient failures, and returns the final
// response. The caller owns the returned response's Body and must close
// it, exactly as with http.Client.Do.
//
// Retries happen on a 429, on any 5xx, and on a network-level error
// (refused connection, DNS failure, reset), up to Config.MaxRetries
// additional attempts. Any other status — including every 4xx other than
// 429 — is returned as-is on the first attempt: a 404 or a 403 is a fact
// about the request, and repeating it just multiplies load on a server
// that already answered clearly. Exhausting the retries on a status code
// returns that last response with a nil error (a status is not an error
// in net/http's contract, and the caller still needs its body and
// headers); exhausting them on a network error returns that error.
//
// The backoff between attempts is exponential with jitter, unless the
// response carried a usable Retry-After header, which wins. The wait is
// interruptible: if req's context is canceled or its deadline passes,
// Do returns that context's error promptly instead of sleeping out the
// remaining backoff, and does not attempt again.
//
// # Only for requests with no body, or a replayable one
//
// Do is safe for GET and HEAD, which is all this project issues. It does
// not rewind request bodies: if req.Body is non-nil and req.GetBody is
// nil, the first attempt consumes the body and any retry would resend
// the request with an empty one — silently, since neither net/http nor
// this package can detect it. Do not pass such a request; if a future
// caller needs retried POSTs, give the request a GetBody (as
// http.NewRequest does for the common body types) and teach Do to use
// it, rather than relying on luck that the retry path is never taken.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}

	ctx := req.Context()
	maxRetries := max(c.cfg.MaxRetries, 0)
	if noRetry, _ := ctx.Value(noRetryKey).(bool); noRetry {
		maxRetries = 0
	}

	for attempt := 0; ; attempt++ {
		// Checked before every attempt, not just before the first: a
		// context that expired during the previous backoff must not
		// produce one more round trip.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("httpclient: %s %s: %w", req.Method, req.URL.Redacted(), err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// A canceled context, an exhausted deadline and a redirect
			// loop are all settled answers, not transient conditions.
			//
			// Note that Config.Timeout firing reports itself as
			// context.DeadlineExceeded too (net/http wraps it that way),
			// so it lands here as well. That is deliberate: a host that
			// could not answer within the whole configured budget is
			// unlikely to answer within the same budget moments later,
			// and retrying it would multiply the caller's worst-case
			// wall clock by MaxRetries+1.
			if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, ErrTooManyRedirects) ||
				errors.Is(err, ErrRedirectRejected) {
				return nil, fmt.Errorf("httpclient: %w", err)
			}
			if attempt >= maxRetries {
				return nil, fmt.Errorf("httpclient: giving up after %d attempt(s): %w", attempt+1, err)
			}
			if waitErr := c.wait(ctx, attempt, ""); waitErr != nil {
				return nil, fmt.Errorf("httpclient: %s %s: waiting to retry: %w",
					req.Method, req.URL.Redacted(), waitErr)
			}
			continue
		}

		if !retryableStatus(resp.StatusCode) || attempt >= maxRetries {
			resp.Body = newLimitedBody(resp.Body, c.cfg.MaxResponseBytes)
			return resp, nil
		}

		// Read the header before the body is drained and closed; the
		// response itself is no longer usable after this point.
		retryAfter := resp.Header.Get("Retry-After")
		drainAndClose(resp.Body)

		if waitErr := c.wait(ctx, attempt, retryAfter); waitErr != nil {
			return nil, fmt.Errorf("httpclient: %s %s: waiting to retry: %w",
				req.Method, req.URL.Redacted(), waitErr)
		}
	}
}

// wait sleeps for the delay owed before the attempt after the given
// 0-based attempt index, preferring the server's own Retry-After value
// over the computed backoff. It returns the context's error if the
// context ends first, so a canceled caller never waits out a queued
// sleep.
func (c *Client) wait(ctx context.Context, attempt int, retryAfter string) error {
	delay, ok := retryAfterDelay(retryAfter, time.Now())
	if !ok {
		delay = backoffDelay(attempt, rand.Float64())
	}
	return sleep(ctx, delay)
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
}

// backoffDelay returns how long to wait after the given 0-based attempt
// index, given a jitter draw in [0,1).
//
// The delay is exponential (backoffBase doubling per attempt, clamped to
// backoffCap) and then jittered into the top half of that window:
// [d/2, d). Full jitter — [0, d) — is the more common recipe, but it
// makes a retry that waits essentially no time a routine outcome, which
// is the opposite of what a 429 asked for. Keeping the floor at half the
// window preserves the backoff's purpose while still spreading
// concurrent callers out so they do not hammer a recovering host in
// lockstep.
//
// Kept as a pure function of (attempt, jitter) so its shape can be
// tested without waiting on a clock.
func backoffDelay(attempt int, jitter float64) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// Anything past 30 doublings is far beyond the cap anyway, and the
	// guard keeps the shift below int64 overflow.
	window := backoffCap
	if attempt < 30 {
		if d := backoffBase << attempt; d < window {
			window = d
		}
	}

	// Defensive clamp: the caller passes rand.Float64, but a bad draw
	// must not produce a negative or over-cap sleep.
	if jitter < 0 || jitter >= 1 {
		jitter = 0
	}

	half := window / 2
	return half + time.Duration(jitter*float64(half))
}

// retryAfterDelay parses a Retry-After header into a delay, reporting
// false when the header is absent, unparseable, or points at a moment
// that has already passed — in which case the caller falls back to its
// computed backoff rather than retrying instantly on a server that just
// asked it to slow down.
//
// Both forms RFC 9110 allows are accepted: a count of seconds, and an
// HTTP-date. now is a parameter so the date branch is testable.
func retryAfterDelay(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}

	if secs, err := strconv.Atoi(header); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}

	if when, err := http.ParseTime(header); err == nil {
		d := when.Sub(now)
		if d < 0 {
			return 0, false
		}
		return d, true
	}

	return 0, false
}

// sleep waits for d, or until ctx ends, whichever comes first. A
// non-positive d still observes an already-ended context, so a canceled
// caller cannot slip one more attempt through on a zero-length wait.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drainAndClose consumes a bounded prefix of a discarded response body
// before closing it, which is what lets the transport reuse the
// connection for the retry instead of opening a new one. Errors are
// ignored on purpose: if the drain fails, the transport simply will not
// reuse the connection, which is not a failure the caller can act on.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainLimit))
	_ = body.Close()
}

// limitedBody enforces a byte ceiling on a response body without
// truncating silently. It reads through an io.LimitReader set one byte
// past the limit: that extra byte is what distinguishes "the body is
// exactly at the limit" (which must end in a normal io.EOF) from "the
// body is longer than the limit" (which must end in
// ErrResponseTooLarge).
type limitedBody struct {
	reader   io.Reader
	closer   io.Closer
	limit    int64
	read     int64
	tooLarge bool
}

func newLimitedBody(body io.ReadCloser, limit int64) io.ReadCloser {
	if limit <= 0 {
		return body
	}
	return &limitedBody{
		reader: io.LimitReader(body, limit+1),
		closer: body,
		limit:  limit,
	}
}

// Read returns the bytes read so far alongside ErrResponseTooLarge on
// the read that crosses the limit, never the sentinel byte itself, so a
// caller that wants the truncated prefix still has it. The error is
// sticky: once over the limit, every subsequent Read reports it too,
// rather than letting a later io.EOF from the drained LimitReader look
// like a clean end of body.
func (b *limitedBody) Read(p []byte) (int, error) {
	if b.tooLarge {
		return 0, ErrResponseTooLarge
	}

	n, err := b.reader.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		// b.read was at most b.limit before this Read, so dropping the
		// overshoot can never make n negative.
		n -= int(b.read - b.limit)
		b.read = b.limit
		b.tooLarge = true
		return n, ErrResponseTooLarge
	}

	return n, err
}

// Close closes the underlying body. The wrapper never takes that
// responsibility away from the caller — net/http's contract is unchanged.
func (b *limitedBody) Close() error {
	return b.closer.Close()
}
