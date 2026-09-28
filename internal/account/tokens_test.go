package account

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestNewToken_ShapeAndDigest(t *testing.T) {
	tok, hash, err := newToken()
	if err != nil {
		t.Fatalf("newToken() error = %v", err)
	}
	if len(tok) != 43 {
		t.Errorf("len(token) = %d, want 43 (256 bits, base64url without padding)", len(tok))
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("token %q is not URL-safe", tok)
	}
	want := sha256.Sum256([]byte(tok))
	if !bytes.Equal(hash, want[:]) {
		t.Errorf("hash is not SHA-256 of the token")
	}
	if bytes.Contains(hash, []byte(tok)) {
		t.Errorf("hash contains the token itself")
	}
	if err := checkToken(tok); err != nil {
		t.Errorf("checkToken(newToken()) = %v, want nil", err)
	}
	if !bytes.Equal(hashToken(tok), hash) {
		t.Errorf("hashToken(token) differs from the digest newToken returned")
	}
}

func TestNewToken_NoRepeats(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		tok, _, err := newToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatalf("token repeated after %d draws", i)
		}
		seen[tok] = true
	}
}

func TestCheckToken(t *testing.T) {
	valid := strings.Repeat("A", 43)
	if err := checkToken(valid); err != nil {
		t.Fatalf("checkToken(%q) = %v, want nil", valid, err)
	}
	bad := map[string]string{
		"empty":                "",
		"too short":            strings.Repeat("A", 42),
		"too long":             strings.Repeat("A", 44),
		"very long":            strings.Repeat("A", 5000),
		"plus":                 strings.Repeat("A", 42) + "+",
		"slash":                strings.Repeat("A", 42) + "/",
		"padding":              strings.Repeat("A", 42) + "=",
		"space":                strings.Repeat("A", 42) + " ",
		"newline":              strings.Repeat("A", 42) + "\n",
		"non-ASCII":            strings.Repeat("A", 42) + "é",
		"non-canonical bits":   strings.Repeat("A", 42) + "B", // last character carries bits that must be zero
		"sql-ish":              "' OR 1=1 --" + strings.Repeat("A", 32),
		"token= prefix pasted": "token=" + strings.Repeat("A", 37),
	}
	for name, tok := range bad {
		if err := checkToken(tok); err == nil {
			t.Errorf("%s: checkToken(%q) = nil, want an error", name, tok)
		}
	}
}
