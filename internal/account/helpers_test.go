package account

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; run `docker compose up -d db` and export TEST_DATABASE_URL to run this test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL does not parse as a URL: %v", err)
	}
	if name := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(name, "_test") {
		t.Fatalf("TEST_DATABASE_URL points at database %q; these tests drop tables and must run against a database whose name ends in \"_test\"", name)
	}
	return raw
}

// newPool resets the schema and returns a pool with room for the concurrency tests.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testDatabaseURL(t)
	if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}
	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	t.Cleanup(func() {
		if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
			t.Errorf("cleanup MigrateDown: %v", err)
		}
	})
	db, err := database.New(context.Background(), config.DatabaseConfig{
		URL: url, MaxConns: 40, MinConns: 0, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(db.Close)
	return db.Pool
}

// testClock is a clock tests move by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Now().UTC().Truncate(time.Second)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var seedCounter atomic.Int64

// seedCompany creates a company with one target board and returns their ids.
func seedCompany(t *testing.T, pool *pgxpool.Pool) (companyID, targetID int64) {
	t.Helper()
	ctx := context.Background()
	n := seedCounter.Add(1)
	if err := pool.QueryRow(ctx, `INSERT INTO companies (name) VALUES ($1) RETURNING id`, fmt.Sprintf("Acme %d", n)).Scan(&companyID); err != nil {
		t.Fatalf("seeding a company: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO target_companies (company_id, ats_provider, external_board_id) VALUES ($1, 'greenhouse', $2) RETURNING id`,
		companyID, fmt.Sprintf("acme-%d", n)).Scan(&targetID); err != nil {
		t.Fatalf("seeding a target: %v", err)
	}
	return companyID, targetID
}

// seedJob inserts a job in the given status ("open", "closed", "removed") and returns its id.
func seedJob(t *testing.T, pool *pgxpool.Pool, status string) int64 {
	t.Helper()
	companyID, targetID := seedCompany(t, pool)
	n := seedCounter.Add(1)
	closedAt := "NULL"
	if status != "open" {
		closedAt = "now()"
	}
	var id int64
	err := pool.QueryRow(context.Background(), fmt.Sprintf(`INSERT INTO jobs
		(company_id, target_company_id, source, source_job_id, title, application_url, content_hash, status, closed_at, published_at)
		VALUES ($1, $2, 'greenhouse', $3, $4, 'https://example.com/apply', 'h', $5, %s, now()) RETURNING id`, closedAt),
		companyID, targetID, fmt.Sprintf("job-%d", n), fmt.Sprintf("Engineer %d", n), status).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a job: %v", err)
	}
	return id
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("counting rows (%s): %v", query, err)
	}
	return n
}

// mustLogin signs an address in through the store and returns the user and the session token.
func mustLogin(t *testing.T, s *Store, email string) (User, string, []byte) {
	t.Helper()
	ctx := context.Background()
	_, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLoginToken(ctx, email, hash, 10*time.Minute); err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}
	sess, sessHash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Login(ctx, hash, sessHash, 14*24*time.Hour, nil, 0)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return u, sess, sessHash
}
