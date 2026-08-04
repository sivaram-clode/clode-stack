package oauthmock

import (
	"sync"
	"time"
)

// grantTTL bounds how long an issued code/token stays resolvable.
const grantTTL = 10 * time.Minute

// Profile is the identity captured on the consent form. Email is the only
// human input; providers derive their own fields (sub, login, numeric id,
// avatar) from it — see derive.go and each provider's userinfo handler.
type Profile struct {
	Email string
	Name  string
}

// grant is a stored authorization, reachable by either the auth code or the
// later access token.
type grant struct {
	profile   Profile
	expiresAt time.Time
}

// store is an in-memory, TTL'd map keyed by both codes and access tokens.
type store struct {
	mu sync.Mutex
	m  map[string]grant
}

func newStore() *store { return &store{m: make(map[string]grant)} }

func (s *store) put(key string, p Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = grant{profile: p, expiresAt: time.Now().Add(grantTTL)}
}

func (s *store) get(key string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.m[key]
	if !ok || time.Now().After(g.expiresAt) {
		delete(s.m, key)
		return Profile{}, false
	}
	return g.profile, true
}
