#!/usr/bin/env bash
# sigv4-put.sh — the canonical "is the CF path working?" regression.
#
# Baseline SigV4 PutObject against MinIO, signed with
# `Accept-Encoding: identity` (what every SDK's default signer emits).
# Two runs:
#   1. Direct to http://localhost:19000  (host publish of clode-minio-1)
#      → curl doesn't add Accept-Encoding; signed value reaches minio
#        unchanged; expect 200.
#   2. Via https://minio.srclode.online   (CF → cloudflared → minio-proxy → minio)
#      → CF rewrites AE identity → gzip,br in transit; minio-proxy
#        rewrites it BACK to identity before minio validates; expect 200.
#
# If direct=200 but CF=403, the proxy is failing to rewrite. See tests/README.md
# for the debugging protocol (X-Proxied-By header, /_proxy/health/full,
# /_proxy/echo, access-log ae_in/ae_out fields).
#
# Hand-rolled SigV4 (openssl HMAC-SHA256 chain) — no aws-sdk-go-v2, no
# python, no other runtime. bash + openssl + curl only.

set -euo pipefail

ACCESS=minioadmin
SECRET=minioadmin
REGION=us-east-1
SERVICE=s3
BUCKET=brahmi-attachments
KEY=repro/$(date +%s).txt
BODY='hello from sigv4 pinpoint'

hex() { xxd -p -c 256 | tr -d '\n'; }
sha256() { printf '%s' "$1" | openssl dgst -sha256 -binary | hex; }
hmac()   { openssl dgst -sha256 -mac HMAC -macopt "$1" -binary; }

sign_and_put() {
  local endpoint=$1 host=$2 label=$3

  local amzdate datestamp payload_hash
  amzdate=$(date -u +%Y%m%dT%H%M%SZ)
  datestamp=$(date -u +%Y%m%d)
  payload_hash=$(sha256 "$BODY")

  # Canonical headers — signed in this exact set. Accept-Encoding is the
  # header CF mangles; we include and sign it to match aws-sdk-go-v2's
  # behavior.
  local canonical_uri="/${BUCKET}/${KEY}"
  local canonical_headers="accept-encoding:identity
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
  echo "signed Accept-Encoding: identity"
  echo

  local resp_headers="/tmp/sigv4-hdr.$$"
  local resp_body="/tmp/sigv4-body.$$"
  set +e
  curl -sS -X PUT "${endpoint}${canonical_uri}" \
    -H "Host: ${host}" \
    -H "Accept-Encoding: identity" \
    -H "X-Amz-Content-Sha256: ${payload_hash}" \
    -H "X-Amz-Date: ${amzdate}" \
    -H "Authorization: ${auth}" \
    --data-binary "$BODY" \
    -D "$resp_headers" \
    -o "$resp_body" \
    -w 'status=%{http_code}\n'
  set -e
  echo "-- response headers (server/cf-*/x-amz-*): --"
  grep -iE '^(HTTP|Server|cf-|x-amz-|Content-Type)' "$resp_headers" || true
  echo "-- response body: --"
  cat "$resp_body"; echo
  rm -f "$resp_headers" "$resp_body"
}

# 1) Direct to minio via host port publish (no CF in path).
sign_and_put 'http://localhost:19000' 'localhost:19000' 'DIRECT — no CF'

# 2) Through cloudflared → CF edge → tunnel → minio.
sign_and_put 'https://minio.srclode.online' 'minio.srclode.online' 'VIA CF — expect 200 (minio-proxy restores AE=identity)'
