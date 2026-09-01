// Package auth turns a password into a session and a session back into a user.
//
// It owns three things the rest of the service never touches: the password
// hashing parameters, the session token (the storage layer only ever sees its
// SHA-256), and the middleware that decides who is making a request.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/foodibd/socialstats/internal/domain"
)

// HashParams are the argon2id cost parameters.
//
// argon2id rather than bcrypt because it is memory-hard: bcrypt's work factor
// costs an attacker CPU, which GPUs and ASICs supply cheaply, while argon2's
// memory cost is the one resource that stays expensive to parallelise.
type HashParams struct {
	// Time is the number of passes.
	Time uint32

	// Memory is the KiB of memory each hash allocates.
	Memory uint32

	// Threads is the parallelism within one hash.
	Threads uint8

	// SaltLength and KeyLength are in bytes.
	SaltLength uint32
	KeyLength  uint32
}

// DefaultHashParams follow current OWASP guidance for argon2id.
//
// 64 MiB per hash is a real cost, and it is the point: it is what makes an
// offline attack on a leaked table expensive. It also makes *our* login path
// expensive, which is why Hasher bounds how many hashes run at once and why the
// login endpoint is rate limited — 64 MiB times an unbounded number of
// concurrent logins is a way to run the process out of memory using nothing but
// wrong passwords.
var DefaultHashParams = HashParams{
	Time:       3,
	Memory:     64 * 1024, // 64 MiB
	Threads:    4,
	SaltLength: 16,
	KeyLength:  32,
}

// Hasher hashes and verifies passwords.
type Hasher struct {
	params HashParams

	// slots bounds concurrent hashing. Each in-flight hash holds Memory KiB, so
	// without a bound the memory ceiling is set by however many people happen
	// to sign in at once. Queueing is the right response to that: a login that
	// waits is better than a process that dies.
	slots chan struct{}
}

// NewHasher builds a hasher. maxConcurrent bounds simultaneous hashes; at zero
// it defaults to the number of CPUs, which keeps the memory ceiling
// proportional to the machine the process is actually running on.
func NewHasher(params HashParams, maxConcurrent int) *Hasher {
	if params.Time == 0 {
		params = DefaultHashParams
	}
	if maxConcurrent <= 0 {
		maxConcurrent = max(runtime.NumCPU(), 1)
	}
	return &Hasher{params: params, slots: make(chan struct{}, maxConcurrent)}
}

// MemoryCeilingBytes is the most memory concurrent hashing can hold at once.
// Worth logging at startup: it is a number people are surprised by.
func (h *Hasher) MemoryCeilingBytes() int64 {
	return int64(cap(h.slots)) * int64(h.params.Memory) * 1024
}

func (h *Hasher) acquire() { h.slots <- struct{}{} }
func (h *Hasher) release() { <-h.slots }

// Hash produces an encoded argon2id hash of password.
//
// The encoding carries its own parameters, so raising the cost later does not
// invalidate existing accounts: an old hash still verifies against the
// parameters it was made with, and NeedsRehash spots it so the next successful
// login can quietly upgrade it.
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	h.acquire()
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.Memory, h.params.Threads, h.params.KeyLength)
	h.release()

	return encodeHash(h.params, salt, key), nil
}

// Verify reports whether password matches the encoded hash.
//
// A malformed stored hash is an error, not a mismatch. Treating it as "wrong
// password" would turn a corrupted row into an account that can never be
// signed into and never explains why.
func (h *Hasher) Verify(password, encoded string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	h.acquire()
	got := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Threads, uint32(len(want)))
	h.release()

	// Constant time: a byte-by-byte comparison that returns early leaks how
	// much of the hash matched, one request at a time.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether an encoded hash was made with weaker parameters
// than the current ones, so a successful login can upgrade it.
//
// That moment is the only one where the plaintext is available, which is why
// the upgrade happens there rather than in a migration.
func (h *Hasher) NeedsRehash(encoded string) bool {
	params, _, key, err := decodeHash(encoded)
	if err != nil {
		// Unreadable hashes get replaced at the next opportunity.
		return true
	}
	return params.Time < h.params.Time ||
		params.Memory < h.params.Memory ||
		params.Threads < h.params.Threads ||
		uint32(len(key)) < h.params.KeyLength
}

// dummyHash is verified against when no account exists for the submitted
// address.
//
// Without it, a login for an unknown address returns in microseconds while a
// known one takes the full argon2 cost, and the difference is a reliable
// account-existence oracle that no amount of identical error text hides. It is
// generated once at startup from a random password nobody knows.
var dummyHash string

// SpendTimeOnAMissingAccount performs the same work a real verification would,
// so that "no such account" and "wrong password" cost the same.
func (h *Hasher) SpendTimeOnAMissingAccount(password string) {
	if dummyHash == "" {
		return
	}
	_, _ = h.Verify(password, dummyHash)
}

// InitDummyHash generates the decoy hash. It is called once at startup because
// it costs a full argon2 run.
func (h *Hasher) InitDummyHash() error {
	if dummyHash != "" {
		return nil
	}
	filler := make([]byte, 32)
	if _, err := rand.Read(filler); err != nil {
		return err
	}
	encoded, err := h.Hash(base64.RawStdEncoding.EncodeToString(filler))
	if err != nil {
		return err
	}
	dummyHash = encoded
	return nil
}

// encodeHash renders the standard PHC string for argon2id.
func encodeHash(p HashParams, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

var errMalformedHash = errors.New("malformed password hash")

// decodeHash parses the PHC string back into its parts.
func decodeHash(encoded string) (HashParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, key
	if len(parts) != 6 || parts[0] != "" {
		return HashParams{}, nil, nil, fmt.Errorf("%w: %d fields", errMalformedHash, len(parts))
	}
	if parts[1] != "argon2id" {
		return HashParams{}, nil, nil, fmt.Errorf("%w: algorithm %q is not argon2id", errMalformedHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return HashParams{}, nil, nil, fmt.Errorf("%w: version %q", errMalformedHash, parts[2])
	}
	if version != argon2.Version {
		return HashParams{}, nil, nil, fmt.Errorf("%w: version %d, this build speaks %d", errMalformedHash, version, argon2.Version)
	}

	var p HashParams
	var memory, timeCost uint64
	var threads uint64
	for _, field := range strings.Split(parts[3], ",") {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			return HashParams{}, nil, nil, fmt.Errorf("%w: parameter %q", errMalformedHash, field)
		}
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return HashParams{}, nil, nil, fmt.Errorf("%w: parameter %q", errMalformedHash, field)
		}
		switch name {
		case "m":
			memory = n
		case "t":
			timeCost = n
		case "p":
			threads = n
		default:
			return HashParams{}, nil, nil, fmt.Errorf("%w: unknown parameter %q", errMalformedHash, name)
		}
	}
	if memory == 0 || timeCost == 0 || threads == 0 || threads > 255 {
		return HashParams{}, nil, nil, fmt.Errorf("%w: parameters m=%d,t=%d,p=%d", errMalformedHash, memory, timeCost, threads)
	}
	p.Memory, p.Time, p.Threads = uint32(memory), uint32(timeCost), uint8(threads)

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return HashParams{}, nil, nil, fmt.Errorf("%w: salt: %v", errMalformedHash, err)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return HashParams{}, nil, nil, fmt.Errorf("%w: key: %v", errMalformedHash, err)
	}
	p.SaltLength, p.KeyLength = uint32(len(salt)), uint32(len(key))

	return p, salt, key, nil
}

// Password policy.
//
// Length and a check against the passwords attackers try first, and nothing
// else. Composition rules — an uppercase, a digit, a symbol — do not survive
// contact with people: they produce Password1! and a sticky note, and they
// reject correct-horse-battery-staple. Length is what actually costs an
// attacker something.
const (
	// MinPasswordLength is counted in runes, so a passphrase in a non-Latin
	// script is not penalised for encoding to more bytes.
	MinPasswordLength = 12

	// MaxPasswordLength bounds the work an unauthenticated caller can ask for.
	// argon2's cost is fixed by its parameters, but hashing the input itself is
	// linear, and there is no legitimate 10 MB password.
	MaxPasswordLength = 128
)

// commonPasswords are the ones that appear at the top of every breach corpus.
// A short embedded list catches the passwords that get guessed first without
// shipping a 100 MB dictionary; a deployment that wants the full list should
// put a proper check in front of this.
var commonPasswords = map[string]bool{
	"123456789012": true, "password1234": true, "qwertyuiop12": true,
	"111111111111": true, "123123123123": true, "abcdefghijkl": true,
	"passwordpassword": true, "iloveyou1234": true, "adminadmin12": true,
	"letmein12345": true, "welcome12345": true, "monkey123456": true,
	"qwerty123456": true, "dragon123456": true, "sunshine1234": true,
	"princess1234": true, "football1234": true, "baseball1234": true,
	"trustno1trustno1": true, "changeme1234": true, "password123!": true,
	"administrator": true, "qazwsxedcrfv": true, "1qaz2wsx3edc": true,
	"correcthorsebatterystaple": true,
}

// ValidatePassword checks a candidate against the policy.
//
// email is used to reject a password derived from the address, which is a
// pattern common enough to be worth catching and cheap to check.
func ValidatePassword(password, email string) error {
	length := utf8.RuneCountInString(password)

	switch {
	case length < MinPasswordLength:
		return fmt.Errorf("%w: use at least %d characters", domain.ErrWeakPassword, MinPasswordLength)
	case length > MaxPasswordLength:
		return fmt.Errorf("%w: use at most %d characters", domain.ErrWeakPassword, MaxPasswordLength)
	}

	lower := strings.ToLower(password)
	if commonPasswords[lower] {
		return fmt.Errorf("%w: that password appears in breach lists, pick another", domain.ErrWeakPassword)
	}

	// A password that is just the email, or the part before the @, is not one.
	if email != "" {
		addr := strings.ToLower(strings.TrimSpace(email))
		local, _, _ := strings.Cut(addr, "@")
		if lower == addr || (len(local) >= 4 && strings.Contains(lower, local)) {
			return fmt.Errorf("%w: do not use your email address in your password", domain.ErrWeakPassword)
		}
	}

	// A single repeated character reaches any length requirement without
	// adding anything an attacker has to search.
	if isRepeatedRune(password) {
		return fmt.Errorf("%w: a repeated character is not a password", domain.ErrWeakPassword)
	}
	return nil
}

func isRepeatedRune(s string) bool {
	var first rune
	for i, r := range s {
		if i == 0 {
			first = r
			continue
		}
		if r != first {
			return false
		}
	}
	return s != ""
}
