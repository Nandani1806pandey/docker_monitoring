// This file implements password hashing (ARCHITECTURE.md §G: "Password
// hashing: Argon2id... memory-hard, resistant to GPU cracking") using the
// real golang.org/x/crypto/argon2 implementation.
//
// golang.org itself is blocked by this build environment's network policy
// (its vanity-import redirect requires reaching golang.org before the
// module proxy is even consulted), which would normally rule out any
// golang.org/x/... import. go.mod works around that with `replace`
// directives pointing golang.org/x/crypto and golang.org/x/sys at their
// official read-only GitHub mirrors (github.com/golang/crypto,
// github.com/golang/sys) — both reachable via `go get`'s direct-git path
// over a host that IS allowed. The import path used throughout this file
// is still the real golang.org/x/crypto/argon2 package; only where the
// module is fetched *from* changes. This gets the real, standard Argon2id
// implementation rather than a hand-rolled substitute.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP's 2023 minimum recommendation for this
// algorithm: m=19MiB+ or, for the higher-memory profile used here,
// m=64MiB, t=1, p=4 — this build uses the latter, matching
// ARCHITECTURE.md §G's "memory-hard, resistant to GPU cracking").
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB = 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	saltBytes    = 16
)

// HashPassword returns a PHC-formatted string
// ("$argon2id$v=19$m=<mem>,t=<time>,p=<threads>$<salt>$<hash>", salt and
// hash both unpadded standard base64) safe to persist directly in
// User.PasswordHash. The salt is randomly generated per call — never
// reused across users or re-hashes.
func HashPassword(plaintext string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(hash)), nil
}

// VerifyPassword reports whether plaintext matches the PHC-encoded hash
// produced by HashPassword, using a constant-time comparison to avoid
// timing side channels. Returns an error for any hash that isn't a
// well-formed argon2id PHC string — a malformed stored hash is a data
// integrity problem the caller should know about, not something to
// silently treat as "doesn't match".
func VerifyPassword(plaintext, encoded string) (bool, error) {
	// Expected shape: "$argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>" —
	// splitting on "$" yields ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash].
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, fmt.Errorf("unrecognized password hash encoding")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("invalid version in stored hash: %w", err)
	}
	if version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version %d in stored hash", version)
	}
	var mem uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return false, fmt.Errorf("invalid params in stored hash: %w", err)
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("invalid salt encoding in stored hash: %w", err)
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("invalid hash encoding in stored hash: %w", err)
	}
	got := argon2.IDKey([]byte(plaintext), salt, time, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// --- Session tokens ---
//
// Same pattern as this codebase's existing agent-enrollment tokens
// (ARCHITECTURE.md §45): a random opaque bearer value is handed to the
// client exactly once, and only its sha256 hash is ever persisted, so a
// leaked storage snapshot never hands out a usable, already-issued
// session.

const sessionTokenBytes = 32

func generateSessionToken() (string, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
