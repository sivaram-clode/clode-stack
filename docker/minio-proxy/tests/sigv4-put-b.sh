#!/usr/bin/env bash
# Confirmation run. If Accept-Encoding is the CF-mangled header, then:
#   • Signing "Accept-Encoding: gzip, br"  and sending "identity"
#     → direct-to-minio: mismatch → 403
#     → via CF: CF rewrites what we sent from identity → gzip, br,
#               which matches what we signed → 200 ✓
# That's the mirror of the first run: the pass/fail pattern flips, and
# ONLY Accept-Encoding's value changed. Nails it to that header.

set -euo pipefail
ACCESS=minioadmin; SECRET=minioadmin
REGION=us-east-1; SERVICE=s3
BUCKET=brahmi-attachments
KEY=repro/$(date +%s)-b.txt
BODY='hello from sigv4 pinpoint (run b)'

hex() { xxd -p -c 256 | tr -d '\n'; }
sha256() { printf '%s' "$1" | openssl dgst -sha256 -binary | hex; }
hmac()   { openssl dgst -sha256 -mac HMAC -macopt "$1" -binary; }

run() {
  local endpoint=$1 host=$2 label=$3
  local amzdate datestamp payload_hash
  amzdate=$(date -u +%Y%m%dT%H%M%SZ)
  datestamp=$(date -u +%Y%m%d)
  payload_hash=$(sha256 "$BODY")

  local canonical_uri="/${BUCKET}/${KEY}"
  # Signed value = what CF forwards (gzip, br), NOT what we transmit.
  local canonical_headers="accept-encoding:gzip, br
host:${host}
x-amz-content-sha256:${payload_hash}
x-amz-date:${amzdate}
"
  local signed_headers="accept-encoding;host;x-amz-content-sha256;x-amz-date"
  local canonical_request="PUT
${canonical_uri}

${canonical_headers}
${signed_headers}
${payload_hash}"

  local cr_hash string_to_sign scope
  cr_hash=$(sha256 "$canonical_request")
  scope="${datestamp}/${REGION}/${SERVICE}/aws4_request"
  string_to_sign="AWS4-HMAC-SHA256
${amzdate}
${scope}
${cr_hash}"

  local kDate kRegion kService kSigning signature
  kDate=$(printf '%s' "$datestamp" | hmac "key:AWS4${SECRET}" | hex)
  kRegion=$(printf '%s' "$REGION"  | hmac "hexkey:${kDate}"  | hex)
  kService=$(printf '%s' "$SERVICE" | hmac "hexkey:${kRegion}" | hex)
  kSigning=$(printf '%s' "aws4_request" | hmac "hexkey:${kService}" | hex)
  signature=$(printf '%s' "$string_to_sign" | hmac "hexkey:${kSigning}" | hex)
  local auth="AWS4-HMAC-SHA256 Credential=${ACCESS}/${scope}, SignedHeaders=${signed_headers}, Signature=${signature}"

  echo
  echo "══════════ ${label} ══════════"
  echo "endpoint: ${endpoint}"
  echo "signed Accept-Encoding: 'gzip, br'   (what CF ACTUALLY sends)"
  echo "wire  Accept-Encoding: 'identity'    (what SDK originally emits)"
  # curl -H "Accept-Encoding: identity" sends identity on the wire. If
  # this path stays 'identity' end-to-end (direct), minio hashes identity
  # ≠ our signed 'gzip, br' → 403. If CF rewrites → gzip, br, minio
  # hashes gzip, br = signed → 200.
  set +e
  curl -sS -X PUT "${endpoint}${canonical_uri}" \
    -H "Host: ${host}" \
    -H "Accept-Encoding: identity" \
    -H "X-Amz-Content-Sha256: ${payload_hash}" \
    -H "X-Amz-Date: ${amzdate}" \
    -H "Authorization: ${auth}" \
    --data-binary "$BODY" \
    -o /tmp/sigv4b-body.$$ \
    -w 'status=%{http_code}\n'
  set -e
  cat /tmp/sigv4b-body.$$; echo
  rm -f /tmp/sigv4b-body.$$
}

run 'http://localhost:19000'         'localhost:19000'     'DIRECT — expect 403 (we signed gzip,br but wire=identity)'
run 'https://minio.srclode.online'   'minio.srclode.online' 'VIA CF   — expect 200 (CF rewrites identity→gzip,br so wire=signed)'
