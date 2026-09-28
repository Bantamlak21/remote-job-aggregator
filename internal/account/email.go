// Package account is passwordless sign-in and saved jobs: users, single-use
// login links, sessions and bookmarks. Secrets (login tokens, session tokens)
// are 256 random bits and only their SHA-256 digests are stored.
package account

import (
	"errors"
	"strings"
)

// ErrInvalidEmail is returned for an address that cannot be used to sign in.
var ErrInvalidEmail = errors.New("account: invalid email address")

const (
	maxEmailLen = 254
	maxLocalLen = 64
)

// NormalizeEmail returns the canonical form of an address: trimmed, and both the
// domain and the local part lower-cased. It accepts only a plain, ASCII addr-spec
// (no display name, no quoted local part, no comments, no address literal, no
// non-ASCII local part, and no internationalized domain, punycode-encoded or not:
// see validDomain) so that what is stored and mailed to is exactly what the person
// typed, and two spellings of one mailbox cannot become two accounts. Plus-addressing
// is kept (a+b@x.com and a@x.com are different addresses here: whether they are one
// mailbox is the mail provider's rule, not ours).
func NormalizeEmail(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > maxEmailLen {
		return "", ErrInvalidEmail
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e { // control characters, spaces, DEL and everything non-ASCII
			return "", ErrInvalidEmail
		}
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.Count(s, "@") != 1 {
		return "", ErrInvalidEmail
	}
	local, domain := s[:at], s[at+1:]
	if len(local) > maxLocalLen || !validLocal(local) || !validDomain(domain) {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(local) + "@" + strings.ToLower(domain), nil
}

// validLocal accepts RFC 5322 atext with dots between atoms: the address forms
// real mailboxes use, and none of the forms (quotes, spaces, comments, control
// characters, non-ASCII) that make header and SMTP injection possible.
func validLocal(l string) bool {
	if l[0] == '.' || l[len(l)-1] == '.' || strings.Contains(l, "..") {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+/=?^_`{|}~.-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// validDomain accepts an ASCII host name with at least two labels, and rejects address
// literals, all-numeric names (IP addresses), and a punycode ("xn--") label: an
// internationalized domain would otherwise be ASCII bytes standing in for a non-ASCII
// identity, which the package's ASCII-only policy means to refuse outright, not to wave
// through as one more ASCII domain (an "xn--" label can encode a homograph of a real
// domain a person would otherwise trust).
func validDomain(d string) bool {
	if len(d) > 253 || !strings.Contains(d, ".") || d[len(d)-1] == '.' {
		return false
	}
	allNumeric := true
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		// A punycode ("xn--") label is ASCII bytes standing in for a non-ASCII identity
		// (an internationalized domain name), which the ASCII-only rule above means to
		// refuse, not merely to check byte-by-byte: gmail.com and its Cyrillic-homograph
		// punycode encoding must not be treated as one more ASCII domain among many.
		if len(label) >= 4 && (label[0]|0x20) == 'x' && (label[1]|0x20) == 'n' && label[2] == '-' && label[3] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i] | 0x20 // lower-case letters
			switch {
			case c >= 'a' && c <= 'z':
				allNumeric = false
			case label[i] >= '0' && label[i] <= '9':
			case label[i] == '-':
				allNumeric = false
			default:
				return false
			}
		}
	}
	return !allNumeric
}

// throttleKey is the address with its aliases folded, for rate limiting only: the
// stored and mailed address is never changed. Any "+tag" in the local part is dropped
// (name+jobs@x.com is name@x.com to nearly every provider), and for Gmail, which
// ignores dots and treats googlemail.com as gmail.com, the dots go too. So
// a.b+1@gmail.com, ab+2@gmail.com and a.b@googlemail.com share one budget instead of
// being a way to send unlimited mail to one mailbox. The input is a normalized address.
func throttleKey(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return email
	}
	local, domain := email[:at], email[at+1:]
	if i := strings.IndexByte(local, '+'); i > 0 {
		local = local[:i]
	}
	if domain == "gmail.com" || domain == "googlemail.com" {
		local = strings.ReplaceAll(local, ".", "")
		domain = "gmail.com"
	}
	return local + "@" + domain
}
