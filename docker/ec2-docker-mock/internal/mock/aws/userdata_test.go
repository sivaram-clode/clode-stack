package aws

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestParseUserData_BrahmiShape pins the actual multipart cloud-init shape
// brahmi emits from internal/vmprovider/userdata.go. The mock must extract
// KEY=val pairs from the base64-encoded write_files entry for agent.env.
func TestParseUserData_BrahmiShape(t *testing.T) {
	agentEnv := "AGENT_ID=abc\nBRAHMI_URL=https://brahmi.example\nAGENT_IMAGE=ghcr.io/clode-labs/benji-vm:latest\n"
	envB64 := base64.StdEncoding.EncodeToString([]byte(agentEnv))

	// Replicate the multipart shape from brahmi/internal/vmprovider/userdata.go.
	multipart := "Content-Type: multipart/mixed; boundary=\"BOUNDARY\"\r\n" +
		"MIME-Version: 1.0\r\n" +
		"\r\n" +
		"--BOUNDARY\r\n" +
		"Content-Type: text/cloud-config; charset=\"us-ascii\"\r\n" +
		"MIME-Version: 1.0\r\n" +
		"\r\n" +
		"#cloud-config\n" +
		"write_files:\n" +
		"  - path: /etc/clode-agent/agent.env\n" +
		"    permissions: '0600'\n" +
		"    owner: 'root:root'\n" +
		"    encoding: b64\n" +
		"    content: " + envB64 + "\n" +
		"\r\n--BOUNDARY--\r\n"

	env := parseUserData(base64.StdEncoding.EncodeToString([]byte(multipart)))
	if env["AGENT_ID"] != "abc" {
		t.Errorf("AGENT_ID=%q want abc", env["AGENT_ID"])
	}
	if env["BRAHMI_URL"] != "https://brahmi.example" {
		t.Errorf("BRAHMI_URL=%q want https://brahmi.example", env["BRAHMI_URL"])
	}
	if env["AGENT_IMAGE"] != "ghcr.io/clode-labs/benji-vm:latest" {
		t.Errorf("AGENT_IMAGE=%q missing", env["AGENT_IMAGE"])
	}
}

func TestParseUserData_MissingIsSafe(t *testing.T) {
	if env := parseUserData(""); env != nil {
		t.Errorf("empty user-data should return nil, got %v", env)
	}
	if env := parseUserData("not-base64-!@#$%"); env != nil {
		t.Errorf("garbage user-data should return nil, got %v", env)
	}
}

func TestParseEnvFile(t *testing.T) {
	body := "# comment\nA=1\nB=\"two words\"\n\nC=three\n"
	got := parseEnvFile(body)
	want := map[string]string{"A": "1", "B": "two words", "C": "three"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%q want %q", k, got[k], v)
		}
	}
	// Make sure the blank line and comment didn't produce phantom keys.
	if len(got) != len(want) {
		t.Errorf("extra keys: %v", got)
	}
	// Comments never survive.
	for k := range got {
		if strings.HasPrefix(k, "#") {
			t.Errorf("comment key survived: %q", k)
		}
	}
}

func TestParseEnvFile_QuotedEmpty(t *testing.T) {
	got := parseEnvFile(`A=""`)
	if _, ok := got["A"]; !ok {
		t.Errorf("A=%q missing", got["A"])
	}
	if got["A"] != "" {
		t.Errorf("A=%q want empty", got["A"])
	}
}
