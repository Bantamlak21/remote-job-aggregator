// Package robots decides whether this crawler may fetch a URL under the
// site's robots.txt (RFC 9309). Every page fetch that is not a documented
// public API — company career pages, job detail pages — goes through
// Checker.Allowed first; a site that says no is not fetched, and a site
// whose robots.txt cannot be read is treated as "no" rather than assumed
// open.
package robots

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Doer is the slice of *httpclient.Client (or any http client) Checker
// needs. Defined here, on the consumer side, so tests use a fake.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ErrUnavailable is returned (wrapped) when a site's robots.txt could not
// be read: a network error or a 5xx. RFC 9309 section 2.3.1.4 says to
// treat that as "complete disallow", so callers skip the URL.
var ErrUnavailable = errors.New("robots: robots.txt unavailable")

// maxRobotsBytes bounds how much of a robots.txt is parsed. RFC 9309
// requires parsers to handle at least 500 KiB; anything past this is
// ignored, not fatal.
const maxRobotsBytes = 512 * 1024

// Checker answers Allowed for URLs on any host, fetching and caching each
// host's robots.txt once for the Checker's lifetime (one ingestion run).
// Safe for concurrent use.
type Checker struct {
	http  Doer
	agent string // lower-cased product token, e.g. "remote-job-aggregator"

	mu    sync.Mutex
	hosts map[string]*hostEntry
}

type hostEntry struct {
	once  sync.Once
	rules *rules // nil with err != nil means unavailable
	err   error
}

// New returns a Checker that identifies as agentToken (the product token
// of the User-Agent, e.g. "remote-job-aggregator", without version).
func New(httpClient Doer, agentToken string) *Checker {
	return &Checker{
		http:  httpClient,
		agent: strings.ToLower(strings.TrimSpace(agentToken)),
		hosts: make(map[string]*hostEntry),
	}
}

// Allowed reports whether rawURL may be fetched. An error means the
// answer is unknown (robots.txt unreadable, or rawURL is not an http(s)
// URL) and the caller must not fetch.
func (c *Checker) Allowed(ctx context.Context, rawURL string) (bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false, fmt.Errorf("robots: %q is not an absolute http(s) URL", rawURL)
	}
	origin := u.Scheme + "://" + strings.ToLower(u.Host)

	c.mu.Lock()
	entry, ok := c.hosts[origin]
	if !ok {
		entry = &hostEntry{}
		c.hosts[origin] = entry
	}
	c.mu.Unlock()

	entry.once.Do(func() { entry.rules, entry.err = c.load(ctx, origin) })
	if entry.err != nil {
		return false, entry.err
	}

	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return entry.rules.allows(normalizePercent(path)), nil
}

func (c *Checker) load(ctx context.Context, origin string) (*rules, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, origin, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, origin, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxRobotsBytes))
		if err != nil {
			return nil, fmt.Errorf("%w: %s: reading body: %v", ErrUnavailable, origin, err)
		}
		// NUL bytes mean the file is not a UTF-8/ASCII text file (typically
		// UTF-16, where every line looks like garbage to this parser and
		// every rule would be silently ignored). Fail closed.
		if bytes.IndexByte(body, 0) >= 0 {
			return &rules{list: []rule{{allow: false, pattern: "/"}}}, nil
		}
		return parse(bytes.NewReader(body), c.agent), nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s: status %d", ErrUnavailable, origin, resp.StatusCode)
	default:
		// Other 4xx (404, 410, 401, 403): no robots.txt to obey. RFC 9309
		// section 2.3.1.3 treats every "unavailable" 4xx as "no rules".
		return &rules{}, nil
	}
}

// rule is one Allow/Disallow line.
type rule struct {
	allow   bool
	pattern string
}

// rules is the merged rule list that applies to this crawler.
type rules struct {
	list []rule
}

// parse reads the groups of a robots.txt and returns the rules of the
// group naming agent, or of the "*" group when none does (RFC 9309
// section 2.2.1: the most specific matching group wins, groups for the
// same agent are merged).
func parse(r io.Reader, agent string) *rules {
	var (
		specific, wildcard []rule
		haveSpecific       bool
		inSpecific, inWild bool
		lastWasAgentLine   bool
		sc                 = bufio.NewScanner(r)
	)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	sc.Split(scanRobotsLines)

	for first := true; sc.Scan(); first = false {
		line := sc.Text()
		if first {
			// A UTF-8 byte order mark before the first line is common and
			// must not glue itself onto "User-agent", which would drop the
			// whole first group (RFC 9309 2.2 allows a BOM).
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent":
			if !lastWasAgentLine {
				inSpecific, inWild = false, false
			}
			lastWasAgentLine = true
			v := strings.ToLower(value)
			switch {
			case v == "*":
				inWild = true
			// The group must name exactly our product token (RFC 9309
			// 2.2.1). A prefix match would let a group for "r" or "remote"
			// govern us, and its "Allow: /" would then override the "*"
			// group's Disallow.
			case v != "" && agent != "" && v == agent:
				inSpecific, haveSpecific = true, true
			}
		case "allow", "disallow":
			lastWasAgentLine = false
			// An empty Disallow means "allow everything" and adds no rule.
			if value == "" {
				continue
			}
			rl := rule{allow: key == "allow", pattern: normalizePercent(value)}
			if inSpecific {
				specific = append(specific, rl)
			}
			if inWild {
				wildcard = append(wildcard, rl)
			}
		default:
			lastWasAgentLine = false
		}
	}

	// A line longer than the scanner buffer stops the scan, so every rule
	// after it would be silently dropped, quietly turning "disallowed" into
	// "allowed". Fail closed instead.
	if err := sc.Err(); err != nil {
		return &rules{list: []rule{{allow: false, pattern: "/"}}}
	}

	if haveSpecific {
		return &rules{list: specific}
	}
	return &rules{list: wildcard}
}

// scanRobotsLines splits on "\n", "\r\n" and a lone "\r" (RFC 9309 allows
// all three; bufio.ScanLines knows only the first two, and a CR-only file
// would arrive as one enormous line whose rules are all ignored).
func scanRobotsLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		switch b {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			return 0, nil, nil // need one more byte to tell "\r" from "\r\n"
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// normalizePercent puts a path or pattern into one canonical percent-
// encoding so "/jobs/%7Efoo", "/jobs/%7efoo" and "/jobs/~foo" compare
// equal: escapes of unreserved characters (letters, digits, "-._~") are
// decoded, and every other escape is upper-cased. Without it a robots.txt
// written one way silently fails to match a URL written the other.
func normalizePercent(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			v := unhex(s[i+1])<<4 | unhex(s[i+2])
			if isUnreserved(v) {
				b.WriteByte(v)
			} else {
				b.WriteByte('%')
				b.WriteString(strings.ToUpper(s[i+1 : i+3]))
			}
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// allows applies RFC 9309's longest-match rule: the matching pattern with
// the most characters wins, and Allow beats Disallow on a tie. No matching
// rule means allowed.
func (rs *rules) allows(path string) bool {
	best, bestLen := true, -1
	for _, rl := range rs.list {
		if !matches(rl.pattern, path) {
			continue
		}
		l := len(rl.pattern)
		if l > bestLen || (l == bestLen && rl.allow) {
			best, bestLen = rl.allow, l
		}
	}
	return best
}

// matches reports whether path starts with pattern, where '*' matches any
// run of characters and a trailing '$' anchors the end.
func matches(pattern, path string) bool {
	if anchored, ok := strings.CutSuffix(pattern, "$"); ok {
		return globMatch(anchored, path)
	}
	// Not anchored: a prefix match, i.e. the pattern followed by "*".
	return globMatch(pattern+"*", path)
}

// globMatch matches s against p, where '*' matches any run of characters.
// Iterative with single-star backtracking, so a hostile robots.txt full of
// '*' cannot make matching exponential (O(len(p)*len(s)) worst case).
func globMatch(p, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && p[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
