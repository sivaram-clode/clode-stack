package aws

import (
	"bufio"
	"encoding/base64"
	"log"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"

	"gopkg.in/yaml.v3"
)

// parseUserData turns the base64-encoded UserData from RunInstances into a
// KEY=value env map. It walks the MIME multipart cloud-init payload brahmi
// emits and specifically extracts the /etc/clode-agent/agent.env file that
// benji-vm's entrypoint expects. Anything else in the payload (the shell
// bootstrap that would restart systemd services) is ignored — the container's
// entrypoint runs those services directly.
//
// Missing or malformed input is treated as "no env" — the container still
// boots; if brahmi truly needed identity in agent.env the running benji will
// fail its own health check, which is the same signal the operator sees on a
// real VM.
func parseUserData(b64 string) map[string]string {
	if b64 == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		log.Printf("aws: parseUserData: not base64: %v", err)
		return nil
	}
	// Try MIME multipart (brahmi's shape) first, fall back to plain shell.
	if env := extractAgentEnvFromMultipart(raw); env != nil {
		return env
	}
	if env := extractAgentEnvFromCloudConfig(raw); env != nil {
		return env
	}
	return nil
}

// extractAgentEnvFromMultipart walks the multipart cloud-init payload and
// looks for the text/cloud-config part carrying `write_files` with a base64
// entry for /etc/clode-agent/agent.env.
func extractAgentEnvFromMultipart(raw []byte) map[string]string {
	// Sniff the Content-Type header if present as the first line of the body.
	header, body, ok := splitCloudInitHeader(raw)
	if !ok {
		return nil
	}
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil
	}
	mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			return nil
		}
		partType, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if partType == "text/cloud-config" {
			data, err := readAll(p)
			if err != nil {
				continue
			}
			if env := extractAgentEnvFromCloudConfig(data); env != nil {
				return env
			}
		}
	}
}

// extractAgentEnvFromCloudConfig parses a cloud-config YAML doc and extracts the
// agent.env write_files entry (base64-encoded).
func extractAgentEnvFromCloudConfig(raw []byte) map[string]string {
	// The cloud-config prefix line is a marker comment; yaml.Unmarshal tolerates it.
	type writeFile struct {
		Path     string `yaml:"path"`
		Encoding string `yaml:"encoding"`
		Content  string `yaml:"content"`
	}
	var doc struct {
		WriteFiles []writeFile `yaml:"write_files"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	for _, f := range doc.WriteFiles {
		if f.Path != "/etc/clode-agent/agent.env" {
			continue
		}
		body := f.Content
		if strings.EqualFold(f.Encoding, "b64") || strings.EqualFold(f.Encoding, "base64") {
			decoded, err := base64.StdEncoding.DecodeString(body)
			if err != nil {
				return nil
			}
			body = string(decoded)
		}
		return parseEnvFile(body)
	}
	return nil
}

// parseEnvFile reads a `KEY=value` env file. Values may be quoted; quotes are
// stripped. Blank lines and `#` comments are skipped.
func parseEnvFile(body string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

// splitCloudInitHeader consumes leading `Header: value` lines up to the first
// blank line and returns the parsed headers + the remaining body.
func splitCloudInitHeader(raw []byte) (textproto.MIMEHeader, []byte, bool) {
	tp := textproto.NewReader(bufio.NewReader(strings.NewReader(string(raw))))
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, nil, false
	}
	// Find where the header block ended in the original bytes so we can
	// return the tail intact.
	i := strings.Index(string(raw), "\r\n\r\n")
	if i < 0 {
		i = strings.Index(string(raw), "\n\n")
		if i < 0 {
			return hdr, nil, true
		}
		return hdr, raw[i+2:], true
	}
	return hdr, raw[i+4:], true
}

func readAll(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
