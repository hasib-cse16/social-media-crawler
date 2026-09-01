package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// tokenBytes is the length of a session token before encoding.
//
// 256 bits. The token is the whole credential — anyone holding it is signed in
// — so it has to be large enough that guessing is not a strategy even against
// an attacker who can try continuously for years.
const tokenBytes = 32

// Token is an opaque session token. It exists in exactly two places: the
// client's cookie, and the memory of the request that minted it. What reaches
// the database is TokenHash(token) and never this.
type Token string

// newToken mints a session token.
func newToken() (Token, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return Token(base64.RawURLEncoding.EncodeToString(buf)), nil
}

// TokenHash is what the sessions table is keyed on.
//
// SHA-256 with no salt or stretching, and that is correct here rather than
// lazy: unlike a password, a token is 256 bits of uniform randomness, so there
// is no dictionary to run and nothing for a work factor to slow down. Its only
// job is to make a leaked database dump useless for signing in, and a plain
// digest does that completely. Stretching would cost time on every
// authenticated request and buy nothing.
func TokenHash(t Token) []byte {
	sum := sha256.Sum256([]byte(t))
	return sum[:]
}

// plausibleToken reports whether a string could be one of our tokens.
//
// It rejects obvious junk before it reaches the database — a scanner sending
// long garbage in a cookie should not become an index lookup each time — while
// deliberately not being a validity check. A token that passes this is still
// only real if the database says so.
func plausibleToken(t Token) bool {
	const encodedLen = (tokenBytes*8 + 5) / 6 // base64 without padding
	if len(t) != encodedLen {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(string(t))
	return err == nil
}

// newCSRFToken mints a CSRF token. It is not secret in the way a session token
// is — it travels in a readable cookie by design — so it only needs to be
// unguessable by a third-party site.
func newCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate csrf token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// sameToken compares two tokens in constant time.
func sameToken(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
