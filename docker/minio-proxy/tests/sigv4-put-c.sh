#!/usr/bin/env bash
# Coverage: confirm Accept-Encoding is the ONLY CF-mangled header that
# breaks SigV4. Sign without Accept-Encoding in the signed-headers list.
# If CF is ALSO rewriting some other header we sign (Host / X-Amz-Date /
# X-Amz-Content-Sha256), this will still fail. If it passes, we know the
# blast radius is exactly one header and the nginx proxy that rewrites
# just that header is sufficient.

set -euo pipefail
ACCESS=minioadmin; SECRET=minioadmin
REGION=us-east-1;  SERVICE=s3
BUCKET=brahmi-attachments
KEY=repro/$(date +%s)-c.txt
BODY='no accept-encoding in signed-headers'

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
  local canonical_headers="host:${host}
x-amz-content-sha256:${payload_hash}
x-amz-date:${amzdate}
"
  local signed_headers="host;x-amz-content-sha256;x-amz-date"
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
  set +e
  curl -sS -X PUT "${endpoint}${canonical_uri}" \
    -H "Host: ${host}" \
    -H "X-Amz-Content-Sha256: ${payload_hash}" \
    -H "X-Amz-Date: ${amzdate}" \
    -H "Authorization: ${auth}" \
    --data-binary "$BODY" \
    -o /tmp/sigv4c-body.$$ \
    -w 'status=%{http_code}\n'
  set -e
  cat /tmp/sigv4c-body.$$; echo
  rm -f /tmp/sigv4c-body.$$
}

run 'http://localhost:19000'       'localhost:19000'      'DIRECT — expect 200'
run 'https://minio.srclode.online' 'minio.srclode.online' 'VIA CF — expect 200 if AE is the ONLY mangled header'
