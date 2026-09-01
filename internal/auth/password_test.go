package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/foodibd/socialstats/internal/domain"
)

// testHashParams keep the tests fast. Real deployments use 64 MiB; running
// that a few dozen times a test run costs seconds and gigabytes for no extra
// coverage, since the parameters are data rather than logic.
var testHashParams = HashParams{Time: 1, Memory: 8 * 1024, Threads: 1, SaltLength: 16, KeyLength: 32}

func testHasher() *Hasher { return NewHasher(testHashParams, 2) }

func TestHashAndVerify(t *testing.T) {
	h := testHasher()
	const password = "correct horse battery staple"

	encoded, err := h.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Errorf("encoding = %q, want the standard PHC form", encoded)
	}

	ok, err := h.Verify(password, encoded)
	if err != nil || !ok {
		t.Errorf("Verify(correct) = %v, %v", ok, err)
	}

	ok, err = h.Verify("not the password", encoded)
	if err != nil {
		t.Fatalf("Verify(wrong): %v", err)
	}
	if ok {
		t.Error("a wrong password verified")
	}
}

// Salting is what stops one cracked hash unlocking every account that shares a
// password, so two hashes of the same input must differ.
func TestHashIsSalted(t *testing.T) {
	h := testHasher()

	first, err := h.Hash("the same password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	second, err := h.Hash("the same password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if first == second {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}

	// Both must still verify.
	for i, encoded := range []string{first, second} {
		if ok, err := h.Verify("the same password", encoded); err != nil || !ok {
			t.Errorf("hash %d did not verify: %v, %v", i, ok, err)
		}
	}
}

// The encoding carries its parameters, so raising the cost later must not lock
// existing accounts out — it must only mark their hashes for upgrade.
func TestOldParametersStillVerifyAndAreMarkedForRehash(t *testing.T) {
	weak := NewHasher(HashParams{Time: 1, Memory: 8 * 1024, Threads: 1, SaltLength: 16, KeyLength: 32}, 1)
	strong := NewHasher(HashParams{Time: 2, Memory: 16 * 1024, Threads: 1, SaltLength: 16, KeyLength: 32}, 1)

	old, err := weak.Hash("a password from last year")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	ok, err := strong.Verify("a password from last year", old)
	if err != nil || !ok {
		t.Fatalf("an old hash stopped verifying after the cost was raised: %v, %v", ok, err)
	}
	if !strong.NeedsRehash(old) {
		t.Error("NeedsRehash did not flag a hash made with weaker parameters")
	}

	fresh, err := strong.Hash("a password from last year")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if strong.NeedsRehash(fresh) {
		t.Error("NeedsRehash flagged a hash made with the current parameters")
	}
	// A hash made with *stronger* parameters than ours must not be downgraded.
	if weak.NeedsRehash(fresh) {
		t.Error("NeedsRehash wanted to downgrade a stronger hash")
	}
}

// A corrupted row must be an error, not a silent "wrong password" that leaves
// an account permanently unusable with no explanation.
func TestVerifyRejectsMalformedHashes(t *testing.T) {
	h := testHasher()

	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"not a hash", "hunter2"},
		{"wrong algorithm", "$argon2i$v=19$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGE"},
		{"bad version", "$argon2id$v=16$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGE"},
		{"missing fields", "$argon2id$v=19$m=8192,t=1,p=1"},
		{"unknown parameter", "$argon2id$v=19$m=8192,t=1,p=1,x=9$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGE"},
		{"zero cost", "$argon2id$v=19$m=0,t=0,p=0$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGE"},
		{"bad base64 salt", "$argon2id$v=19$m=8192,t=1,p=1$!!!!$aGFzaA"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := h.Verify("anything", tc.encoded)
			if err == nil {
				t.Errorf("Verify accepted a malformed hash without complaint (ok=%v)", ok)
			}
			if ok {
				t.Error("a malformed hash verified")
			}
			if !h.NeedsRehash(tc.encoded) {
				t.Error("an unreadable hash should be marked for replacement")
			}
		})
	}
}

func TestMemoryCeiling(t *testing.T) {
	h := NewHasher(HashParams{Time: 1, Memory: 64 * 1024, Threads: 1, SaltLength: 16, KeyLength: 32}, 4)

	// 4 concurrent x 64 MiB. The number people are surprised by when sizing a
	// container, which is why it is computed rather than left implicit.
	if got := h.MemoryCeilingBytes(); got != 4*64*1024*1024 {
		t.Errorf("ceiling = %d bytes, want 256 MiB", got)
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		email    string
		wantErr  bool
		reason   string
	}{
		{"a decent passphrase", "seven blue mountains rise", "a@b.com", false, ""},
		{"exactly the minimum", "abcdefghijkm", "a@b.com", false, ""},
		{"one short", "abcdefghijk", "a@b.com", true, "at least"},
		{"empty", "", "a@b.com", true, "at least"},
		{"absurdly long", strings.Repeat("x", 200), "a@b.com", true, "at most"},
		{"a breach-list favourite", "qwerty123456", "a@b.com", true, "breach"},
		{"breach list, different case", "QWERTY123456", "a@b.com", true, "breach"},
		{"the email itself", "alice@example.com", "alice@example.com", true, "email"},
		{"contains the local part", "alice-is-great-1", "alice@example.com", true, "email"},
		{"a short local part is not matched", "bobbobbobbob", "bo@example.com", false, ""},
		{"one repeated character", "aaaaaaaaaaaaaaaa", "a@b.com", true, "repeated"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password, tc.email)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidatePassword(%q) accepted it", tc.password)
				}
				if !errors.Is(err, domain.ErrWeakPassword) {
					t.Errorf("error = %v, want ErrWeakPassword", err)
				}
				if !strings.Contains(err.Error(), tc.reason) {
					t.Errorf("error = %q, want it to mention %q", err, tc.reason)
				}
				return
			}
			if err != nil {
				t.Errorf("ValidatePassword(%q) = %v, want it accepted", tc.password, err)
			}
		})
	}
}

// Length is counted in runes, so a passphrase in a non-Latin script is not
// penalised for encoding to more bytes than a Latin one.
func TestPasswordLengthIsCountedInRunes(t *testing.T) {
	// Twelve characters, thirty-six bytes.
	const passphrase = "青空青空青空青空青空青緑"
	if got := len(passphrase); got <= MinPasswordLength {
		t.Fatalf("test fixture is %d bytes; it needs to exceed the limit in bytes to be meaningful", got)
	}
	if err := ValidatePassword(passphrase, "a@b.com"); err != nil {
		t.Errorf("a 12-rune passphrase was rejected: %v", err)
	}

	// Eleven runes must still fail, so the check is counting, not skipped.
	if err := ValidatePassword("青空青空青空青空青空緑", "a@b.com"); err == nil {
		t.Error("an 11-rune passphrase was accepted")
	}
}

func TestTokensAreDistinctAndPlausible(t *testing.T) {
	seen := map[Token]bool{}
	for range 100 {
		token, err := newToken()
		if err != nil {
			t.Fatalf("newToken: %v", err)
		}
		if seen[token] {
			t.Fatal("newToken produced a duplicate")
		}
		seen[token] = true

		if !plausibleToken(token) {
			t.Errorf("a freshly minted token %q failed the shape check", token)
		}
	}

	for _, bad := range []Token{"", "short", Token(strings.Repeat("a", 100)), "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"} {
		if plausibleToken(bad) {
			t.Errorf("plausibleToken(%q) = true", bad)
		}
	}
}

// The hash is what reaches storage; the token must not be derivable from it and
// the mapping must be stable.
func TestTokenHashIsStableAndDistinct(t *testing.T) {
	a, _ := newToken()
	b, _ := newToken()

	if len(TokenHash(a)) != 32 {
		t.Errorf("hash is %d bytes, want 32", len(TokenHash(a)))
	}
	if string(TokenHash(a)) != string(TokenHash(a)) {
		t.Error("TokenHash is not deterministic")
	}
	if string(TokenHash(a)) == string(TokenHash(b)) {
		t.Error("two tokens hashed to the same value")
	}
	if strings.Contains(string(TokenHash(a)), string(a)) {
		t.Error("the token is recoverable from its hash")
	}
}

func TestSameTokenIsLengthSafe(t *testing.T) {
	if !sameToken("abc", "abc") {
		t.Error("identical tokens did not match")
	}
	if sameToken("abc", "abd") || sameToken("abc", "abcd") || sameToken("", "a") {
		t.Error("different tokens matched")
	}
}
