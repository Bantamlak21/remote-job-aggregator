package account

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// tokenBytes is the entropy of a login or session token: 256 bits.
const tokenBytes = 32

// maxTokenLen bounds what is even hashed: a valid token is 43 base64url characters.
const maxTokenLen = 64

// newToken returns a random token to hand to the person (base64url, no padding)
// and the digest to store. The token is never stored.
func newToken() (token string, hash []byte, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("account: reading random bytes: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

// hashToken is the stored form of a token.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// errBadToken is returned for a token that cannot be one of ours (wrong length or
// alphabet), before any database work.
var errBadToken = errors.New("account: malformed token")

// checkToken rejects a token that cannot have been issued by newToken.
func checkToken(token string) error {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) || len(token) > maxTokenLen {
		return errBadToken
	}
	// The decoder skips \r and \n, so the alphabet is checked here.
	for i := 0; i < len(token); i++ {
		c := token[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return errBadToken
		}
	}
	if _, err := base64.RawURLEncoding.Strict().DecodeString(token); err != nil {
		return errBadToken
	}
	return nil
}

// maxLookupLen bounds how much of a submitted link token is hashed and looked up.
const maxLookupLen = 256

// clampToken cuts an over-long submitted token down before hashing, so any input costs
// the same to refuse. A real token is 43 characters, far below the cut.
func clampToken(token string) string {
	if len(token) > maxLookupLen {
		return token[:maxLookupLen]
	}
	return token
}
