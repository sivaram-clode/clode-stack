package main

import (
	"crypto/rand"
	"encoding/hex"
	"hash/fnv"
	"strings"
)

// randToken returns a random opaque token for auth codes and access tokens.
func randToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// deriveName turns an email into a plausible display name — providers that
// require a non-empty name reuse this. "bob.smith@x.io" -> "Bob Smith".
func deriveName(email string) string {
	parts := strings.Fields(sanitize(localPart(email)))
	for i, p := range parts {
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	if name := strings.Join(parts, " "); name != "" {
		return name
	}
	return "Mock User"
}

// deriveLogin turns an email into a GitHub-style login (alnum + hyphen):
// "bob.smith@x.io" -> "bob-smith".
func deriveLogin(email string) string {
	var b strings.Builder
	for _, r := range localPart(email) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if login := strings.Trim(b.String(), "-"); login != "" {
		return login
	}
	return "mockuser"
}

// deriveHexID is a deterministic string id from the email (Google's `sub`),
// so the same email always maps to the same user.
func deriveHexID(email string) string {
	return "mock-" + hex.EncodeToString([]byte(email))
}

// deriveNumericID is a deterministic positive int id from the email (GitHub's
// numeric `id`).
func deriveNumericID(email string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(email))
	return h.Sum32()
}

// localPart returns the portion of an email before '@' (or the whole string).
func localPart(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// sanitize replaces common separators with spaces for name derivation.
func sanitize(s string) string {
	return strings.NewReplacer(".", " ", "_", " ", "-", " ", "+", " ").Replace(s)
}
