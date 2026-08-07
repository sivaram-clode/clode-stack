// Package imds is a stand-in for the AWS EC2 Instance Metadata Service, just
// enough of it for an aramb-vm agent's kairo to fetch a signed instance-identity
// document at call-home time. Real EC2 serves this per-instance on the
// link-local 169.254.169.254; locally every agent container instead points its
// IMDS_BASE_URL at THIS group under a per-instance path
// (http://mock-services:8080/imds/<instance-id>), injected by the aws group when
// it launches the container — so the instance id comes from the URL, no
// link-local networking or caller-IP guessing needed.
//
// The document is a genuine PKCS7 SignedData (RSA-2048 / SHA-256), signed with a
// keypair READ from IMDS_IDENTITY_DIR (cert.pem + key.pem). This code never
// generates anything — the operator provides the pair (see scripts/up.py, which
// creates it once into a gitignored host dir bind-mounted here and, read-only,
// into brahmi). Missing or empty files are fatal at boot: the mock is strictly a
// local-testing signer, so failing loud beats signing with a surprise key. brahmi
// verifies against the same cert.pem via AGENT_VM_IDENTITY_CERT_PATH.
package imds

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mozilla.org/pkcs7"
)

// identityDoc mirrors the fields brahmi's verifier extracts from the AWS
// instance-identity document (instanceidentity.Claims). Only these are needed.
type identityDoc struct {
	InstanceID  string `json:"instanceId"`
	AccountID   string `json:"accountId"`
	Region      string `json:"region"`
	PendingTime string `json:"pendingTime"`
}

type signer struct {
	cert      *x509.Certificate
	key       *rsa.PrivateKey
	accountID string
	region    string
}

// Register mounts the IMDS routes. kairo (imds.go) hits, under this group's
// prefix, PUT /latest/api/token (IMDSv2 session token) then GET
// /latest/dynamic/instance-identity/rsa2048. Both are scoped by the :iid path
// segment so the signed document names the calling instance.
func Register(r fiber.Router) {
	s, err := newSigner()
	if err != nil {
		// Fail loud at wiring time — a broken signer means every VM call-home
		// 401s, which is far more confusing to debug downstream.
		log.Fatalf("imds: signer init failed: %v", err)
	}
	r.Put("/:iid/latest/api/token", s.handleToken)
	r.Get("/:iid/latest/dynamic/instance-identity/rsa2048", s.handleIdentity)
}

func newSigner() (*signer, error) {
	dir := envOr("IMDS_IDENTITY_DIR", "/etc/clode/vm-identity")
	cert, key, err := readIdentity(dir)
	if err != nil {
		return nil, err
	}
	return &signer{
		cert:      cert,
		key:       key,
		accountID: envOr("MOCK_AWS_ACCOUNT_ID", "834379228613"),
		region:    envOr("MOCK_AWS_REGION", "eu-west-2"),
	}, nil
}

// readIdentity reads the signing cert.pem + key.pem from dir. It NEVER generates:
// the operator provides the pair (see scripts/up.py's generate hint / the fork
// docs). A missing or empty file is an error — Register turns it into a boot-time
// fatal, since a local-testing signer with no key can only 401 every VM.
func readIdentity(dir string) (*x509.Certificate, *rsa.PrivateKey, error) {
	certPEM, err := readNonEmpty(filepath.Join(dir, "cert.pem"))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := readNonEmpty(filepath.Join(dir, "key.pem"))
	if err != nil {
		return nil, nil, err
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, nil, fmt.Errorf("decode cert PEM in %s", dir)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse cert: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, nil, fmt.Errorf("decode key PEM in %s", dir)
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse key (want PKCS8): %w", err)
	}
	key, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("signing key is not RSA")
	}
	log.Printf("imds: loaded signing identity from %s", dir)
	return cert, key, nil
}

// readNonEmpty reads a file and errors if it is absent or empty (whitespace-only
// counts as empty) — the "path provided but empty ⇒ fatal" contract.
func readNonEmpty(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return b, nil
}

// handleToken answers the IMDSv2 token PUT with any non-empty token — kairo only
// echoes it back on the follow-up GET, which this mock does not require.
func (s *signer) handleToken(c *fiber.Ctx) error {
	return c.Status(fiber.StatusOK).SendString("mock-imds-token")
}

// handleIdentity signs and returns the instance-identity PKCS7 for the :iid in
// the path, base64-encoded exactly like the real /rsa2048 endpoint.
func (s *signer) handleIdentity(c *fiber.Ctx) error {
	iid := strings.TrimSpace(c.Params("iid"))
	if iid == "" {
		return c.Status(fiber.StatusBadRequest).SendString("missing instance id")
	}
	doc := identityDoc{
		InstanceID:  iid,
		AccountID:   s.accountID,
		Region:      s.region,
		PendingTime: time.Now().UTC().Format(time.RFC3339),
	}
	docJSON, err := json.Marshal(doc)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("marshal doc: " + err.Error())
	}
	sd, err := pkcs7.NewSignedData(docJSON)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("new signed data: " + err.Error())
	}
	// SHA-256 to match the /rsa2048 endpoint brahmi verifies against (its pinned
	// path is RSA-2048 / SHA-256, not the DSA-SHA1 /pkcs7 endpoint).
	sd.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	if err := sd.AddSigner(s.cert, s.key, pkcs7.SignerInfoConfig{}); err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("add signer: " + err.Error())
	}
	der, err := sd.Finish()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("finish: " + err.Error())
	}
	return c.Status(fiber.StatusOK).SendString(base64.StdEncoding.EncodeToString(der))
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
