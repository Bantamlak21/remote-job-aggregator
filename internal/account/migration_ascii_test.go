package account

import (
	"context"
	"testing"
)

// F25 (defense in depth): the application already rejects a non-ASCII address before it
// reaches the database (see email_test.go), but the users and login_tokens CHECK
// constraints must refuse one anyway, so a bug in that earlier check is not the only
// thing standing between a non-ASCII address and the database.
func TestMigration_NonASCIIEmailIsRefusedByTheDatabase(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	for _, email := range []string{"josé@example.com", "aａ@example.com", "a@exámple.com"} {
		if _, err := pool.Exec(ctx, `INSERT INTO users (email) VALUES ($1)`, email); err == nil {
			t.Errorf("users accepted the non-ASCII address %q", email)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO login_tokens (token_hash, email, throttle_key, created_at, expires_at) VALUES (decode(repeat('00', 32), 'hex'), $1, $1, now(), now() + interval '10 minutes')`,
			email); err == nil {
			t.Errorf("login_tokens accepted the non-ASCII address %q", email)
		}
	}

	// A plain ASCII address still works, so the constraint is not simply refusing everything.
	if _, err := pool.Exec(ctx, `INSERT INTO users (email) VALUES ('ascii@example.com')`); err != nil {
		t.Errorf("a plain ASCII address was refused: %v", err)
	}
}
