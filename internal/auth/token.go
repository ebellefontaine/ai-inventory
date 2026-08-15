// Package auth provides credential generation and verification primitives.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// GenerateToken returns the plaintext token (shown to the user exactly once)
// and the hash to store. The plaintext is never persisted anywhere.
//
// SHA-256 is correct here rather than bcrypt or argon2: these are 256-bit
// random secrets, not user-chosen passwords, so there is nothing to brute
// force and we need verification to be fast enough to run on every request.
func GenerateToken(prefix string) (plain, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	plain = fmt.Sprintf("%s_%s", prefix, base64.RawURLEncoding.EncodeToString(raw))
	return plain, HashToken(plain), nil
}

// HashToken returns the hex-encoded SHA-256 of the plaintext token. This is
// the only form of the token that is ever stored.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// VerifyToken compares in constant time to avoid leaking the hash by timing.
func VerifyToken(plain, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashToken(plain)), []byte(hash)) == 1
}
