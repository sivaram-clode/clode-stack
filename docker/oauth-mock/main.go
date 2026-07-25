package main

import (
	"log"
	"os"
)

// providers is the set the mock serves. Add a new one here (and a file
// implementing Provider) — nothing else changes.
func providers() []Provider {
	return []Provider{
		GoogleProvider{},
		GitHubProvider{},
	}
}

func main() {
	addr := envOr("LISTEN_ADDR", ":8080")
	app, err := newApp(
		os.Getenv("DEFAULT_EMAIL"),
		envOr("CONSENT_TEMPLATE", "views/consent.html"),
		providers()...,
	)
	if err != nil {
		log.Fatalf("oauth-mock: %v", err)
	}
	log.Printf("oauth-mock listening on %s", addr)
	log.Fatal(app.Listen(addr))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
