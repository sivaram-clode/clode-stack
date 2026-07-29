package aws

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestE2E_LifecycleAgainstRealDocker drives the mock through the full
// Run/Describe/Stop/Start/Hibernate/Terminate lifecycle using a real docker
// daemon. Skipped if DOCKER_HOST isn't reachable so `go test ./...` in CI
// without docker still passes.
func TestE2E_LifecycleAgainstRealDocker(t *testing.T) {
	if os.Getenv("EC2MOCK_SKIP_E2E") != "" {
		t.Skip("EC2MOCK_SKIP_E2E set")
	}
	m, err := New(Config{Network: "bridge"})
	if err != nil {
		t.Skipf("docker not reachable, skipping e2e: %v", err)
	}
	defer func() { _ = m.Close() }()

	srv := httptest.NewServer(m)
	defer srv.Close()

	// A minimal image guaranteed to exist on most laptops; if not, pull is
	// fast. sleep infinity keeps it alive across stop/start.
	// nginx:alpine has an entrypoint that stays alive across `docker start`
	// (nginx runs in the foreground). Plain alpine exits immediately on
	// restart because its default CMD is `sh` and there's no attached tty.
	image := "docker.io/library/nginx:alpine"

	// --- RunInstances ---
	body := "Action=RunInstances&Version=2016-11-15" +
		"&ImageId=" + image +
		"&MinCount=1&MaxCount=1" +
		"&UserData=" + base64.StdEncoding.EncodeToString([]byte("noop")) +
		"&TagSpecification.1.ResourceType=instance" +
		"&TagSpecification.1.Tag.1.Key=env" +
		"&TagSpecification.1.Tag.1.Value=e2e-test"
	resp := postForm(t, srv.URL, body)
	var runResp RunInstancesResponse
	mustDecode(t, resp, &runResp)
	if len(runResp.Instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(runResp.Instances))
	}
	iid := runResp.Instances[0].InstanceID
	t.Logf("launched %s (docker container backing it)", iid)
	// Best-effort cleanup so a failed test doesn't leak a container.
	defer func() {
		_ = postForm(t, srv.URL, "Action=TerminateInstances&InstanceId.1="+iid+"&X-EC2Mock-RemoveVolume=true")
	}()

	// --- DescribeInstances by tag ---
	body = "Action=DescribeInstances&Filter.1.Name=tag%3Aenv&Filter.1.Value.1=e2e-test"
	resp = postForm(t, srv.URL, body)
	var descResp DescribeInstancesResponse
	mustDecode(t, resp, &descResp)
	foundOurs := false
	for _, res := range descResp.Reservations {
		for _, inst := range res.Instances {
			if inst.InstanceID == iid {
				foundOurs = true
				if inst.State.Name != "running" && inst.State.Name != "pending" {
					t.Errorf("expected running/pending, got %q", inst.State.Name)
				}
			}
		}
	}
	if !foundOurs {
		t.Fatalf("instance %s not in Describe result", iid)
	}

	// --- StopInstances (plain) → wait for stopped state ---
	postForm(t, srv.URL, "Action=StopInstances&InstanceId.1="+iid)
	assertState(t, srv.URL, iid, "stopped", 15*time.Second)

	// --- StartInstances → wait for running ---
	postForm(t, srv.URL, "Action=StartInstances&InstanceId.1="+iid)
	assertState(t, srv.URL, iid, "running", 15*time.Second)

	// --- Hibernate (StopInstances Hibernate=true) → mapped to docker pause ---
	postForm(t, srv.URL, "Action=StopInstances&InstanceId.1="+iid+"&Hibernate=true")
	assertState(t, srv.URL, iid, "stopped", 15*time.Second)

	// --- StartInstances from hibernated → docker unpause → running ---
	postForm(t, srv.URL, "Action=StartInstances&InstanceId.1="+iid)
	assertState(t, srv.URL, iid, "running", 15*time.Second)
}

// --- helpers -----------------------------------------------------------------

func postForm(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body=%s", resp.StatusCode, body)
	}
	return resp
}

func mustDecode(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := xml.Unmarshal(body, out); err != nil {
		t.Fatalf("xml decode: %v — body=%s", err, body)
	}
}

func assertState(t *testing.T, url, iid, wantState string, deadline time.Duration) {
	t.Helper()
	stop := time.Now().Add(deadline)
	var lastState string
	for time.Now().Before(stop) {
		resp, err := http.Post(url, "application/x-www-form-urlencoded",
			strings.NewReader("Action=DescribeInstances&InstanceId.1="+iid))
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		var d DescribeInstancesResponse
		mustDecode(t, resp, &d)
		if len(d.Reservations) > 0 && len(d.Reservations[0].Instances) > 0 {
			lastState = d.Reservations[0].Instances[0].State.Name
			if lastState == wantState {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("state never reached %q (last observed %q) within %s", wantState, lastState, deadline)
}
