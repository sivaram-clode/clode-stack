package poolmanager

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestProvisionResolve(t *testing.T) {
	list := ProvisionList{
		Default: map[string]ProvisionEntry{
			"aramb-browser": {Image: "brave-head:latest", Env: map[string]string{"IKKI_GRPC_ADDR": "ikki:9000"}},
		},
		Origins: map[string]map[string]ProvisionEntry{
			"ikkifix": {"aramb-browser": {Image: "brave-head:ikkifix", Env: map[string]string{"IKKI_GRPC_ADDR": "ikki-ikkifix:9000"}}},
		},
	}

	// Origin-specific mapping wins.
	if e, ok := list.resolve("ikkifix", "aramb-browser"); !ok || e.Image != "brave-head:ikkifix" {
		t.Fatalf("fork origin: got (%+v, %v), want image brave-head:ikkifix", e, ok)
	}
	// Unknown origin falls back to default.
	if e, ok := list.resolve("baseline-ikki", "aramb-browser"); !ok || e.Image != "brave-head:latest" {
		t.Fatalf("default fallback: got (%+v, %v), want image brave-head:latest", e, ok)
	}
	// Unknown serviceType → not found.
	if _, ok := list.resolve("ikkifix", "kairo"); ok {
		t.Fatal("unknown serviceType should not resolve")
	}
}

func TestBuildConfigEntryMergesEnv(t *testing.T) {
	svcID := uuid.New()
	entry, ok := buildConfigEntry("aramb-browser", svcID, "aramb-browser-abc", "brave-head:ikkifix",
		map[string]string{"IKKI_GRPC_ADDR": "ikki-ikkifix:9000"}, "agent-1")
	if !ok {
		t.Fatal("expected template for aramb-browser")
	}
	if entry.Settings.Image != "brave-head:ikkifix" {
		t.Fatalf("image not substituted: %q", entry.Settings.Image)
	}
	vars := map[string]string{}
	for _, v := range entry.Vars {
		vars[v.Key] = v.Value
	}
	// Origin env merged into the config's env block.
	if vars["IKKI_GRPC_ADDR"] != "ikki-ikkifix:9000" {
		t.Fatalf("origin env not merged: %q", vars["IKKI_GRPC_ADDR"])
	}
	// Template var preserved, pool identity vars stamped.
	if vars["IKKI_CONNECT"] != "true" {
		t.Fatalf("template var missing: %q", vars["IKKI_CONNECT"])
	}
	if vars["JUMBO_SERVICE_ID"] != svcID.String() || vars["JUMBO_SERVICE_SLUG"] != "aramb-browser-abc" || vars["AGENT_ID"] != "agent-1" {
		t.Fatalf("pool identity vars wrong: %+v", vars)
	}
}

func TestBuildConfigEntryKairoPassword(t *testing.T) {
	entry, ok := buildConfigEntry("kairo", uuid.New(), "kairo-xyz", "benji:latest",
		map[string]string{"BRAHMI_URL": "http://brahmi-fork:8080"}, "agent-2")
	if !ok {
		t.Fatal("expected template for kairo")
	}
	vars := map[string]string{}
	for _, v := range entry.Vars {
		vars[v.Key] = v.Value
	}
	if vars["CODE_SERVER_PASSWORD"] == "" {
		t.Fatal("kairo family must get a CODE_SERVER_PASSWORD")
	}
	if vars["BRAHMI_URL"] != "http://brahmi-fork:8080" {
		t.Fatalf("origin BRAHMI_URL not merged: %q", vars["BRAHMI_URL"])
	}
}

func TestOrgFromToken(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"organization_id": "org-123"})
	tok := "h." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	if got := orgFromToken("Bearer " + tok); got != "org-123" {
		t.Fatalf("org from token: got %q want org-123", got)
	}
	if got := orgFromToken(""); got != "" {
		t.Fatalf("empty header should yield empty org, got %q", got)
	}
	if got := orgFromToken("Bearer not-a-jwt"); got != "" {
		t.Fatalf("garbage token should yield empty org, got %q", got)
	}
}
