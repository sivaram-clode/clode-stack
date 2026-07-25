#!/usr/bin/env bash
# Presigned URL through CF — is it affected by the Accept-Encoding
# rewrite too, or does it slip past because the signed-headers set is
# narrower? Test both GET and PUT presigned URLs against both endpoints.
#
# Key mechanic to prove/disprove: in a presigned URL, SigV4 puts the
# signature in the QUERY STRING (X-Amz-Signature=…), and the
# X-Amz-SignedHeaders=… param tells the server which request headers to
# include in verification. For AWS SDKs, that list is nearly always
# just `host` for presigned URLs — nothing else the sender attaches
# affects verification. If that holds, CF-rewriting Accept-Encoding on
# the way in is irrelevant to a presigned URL — the proxy isn't needed
# for that path at all.
#
# We generate the presigned URL manually (same openssl SigV4 code as
# the earlier repros) with SignedHeaders=host only, then attempt the
# request through direct vs CF.

set -euo pipefail
ACCESS=minioadmin; SECRET=minioadmin
REGION=us-east-1;  SERVICE=s3
BUCKET=brahmi-attachments

hex() { xxd -p -c 256 | tr -d '\n'; }
sha256() { printf '%s' "$1" | openssl dgst -sha256 -binary | hex; }
hmac()   { openssl dgst -sha256 -mac HMAC -macopt "$1" -binary; }

# urlencode a string per RFC3986 (SigV4 needs strict unreserved encoding
# for canonical query construction).
urlenc() {
  local s=$1 out=""
  local i c
  for (( i=0; i<${#s}; i++ )); do
    c=${s:i:1}
    case "$c" in
      [A-Za-z0-9._~-]) out+="$c" ;;
      *) out+=$(printf '%%%02X' "'$c") ;;
    esac
  done
  printf '%s' "$out"
}

presign() {
  local method=$1 host=$2 key=$3 expires=$4 body=$5 scheme=$6

  local amzdate datestamp payload_hash
  amzdate=$(date -u +%Y%m%dT%H%M%SZ)
  datestamp=$(date -u +%Y%m%d)
  # AWS presigned URLs use "UNSIGNED-PAYLOAD" for the payload hash so the
  # sender doesn't have to know the body upfront. MinIO honors this.
  payload_hash="UNSIGNED-PAYLOAD"

  local scope="${datestamp}/${REGION}/${SERVICE}/aws4_request"
  local canonical_uri="/${BUCKET}/${key}"

  # Query params contribute to canonical string in sorted order.
  # SignedHeaders=host is the whole point — nothing else the client
  # sends will be verified.
  local q_credential q_date q_expires q_signedheaders
  q_credential=$(urlenc "${ACCESS}/${scope}")
  q_date="${amzdate}"
  q_expires="${expires}"
  q_signedheaders="host"

  # Canonical query string — params sorted by key, urlencoded values.
  local canonical_query="X-Amz-Algorithm=AWS4-HMAC-SHA256"
  canonical_query+="&X-Amz-Credential=${q_credential}"
  canonical_query+="&X-Amz-Date=${q_date}"
  canonical_query+="&X-Amz-Expires=${q_expires}"
  canonical_query+="&X-Amz-SignedHeaders=${q_signedheaders}"

  local canonical_headers="host:${host}
"
  local canonical_request="${method}
${canonical_uri}
${canonical_query}
${canonical_headers}
${q_signedheaders}
${payload_hash}"

  local cr_hash
  cr_hash=$(sha256 "$canonical_request")
  local string_to_sign="AWS4-HMAC-SHA256
${amzdate}
${scope}
${cr_hash}"

  local kDate kRegion kService kSigning signature
  kDate=$(printf '%s' "$datestamp" | hmac "key:AWS4${SECRET}" | hex)
  kRegion=$(printf '%s' "$REGION"  | hmac "hexkey:${kDate}"  | hex)
  kService=$(printf '%s' "$SERVICE" | hmac "hexkey:${kRegion}" | hex)
  kSigning=$(printf '%s' "aws4_request" | hmac "hexkey:${kService}" | hex)
  signature=$(printf '%s' "$string_to_sign" | hmac "hexkey:${kSigning}" | hex)

  printf '%s://%s%s?%s&X-Amz-Signature=%s\n' \
    "$scheme" "$host" "$canonical_uri" "$canonical_query" "$signature"
}

# ─── Seed an object via a plain direct PUT so we can GET it ───────────
KEY="repro/presign-$(date +%s).txt"
BODY="presign test body"

# Sign a normal request to seed the object; simpler than pre-presigning
# PUT.
seed_put() {
  local host=$1 endpoint=$2 amzdate datestamp payload_hash
  amzdate=$(date -u +%Y%m%dT%H%M%SZ)
  datestamp=$(date -u +%Y%m%d)
  payload_hash=$(sha256 "$BODY")
  local scope="${datestamp}/${REGION}/${SERVICE}/aws4_request"
  local canonical_headers="host:${host}
x-amz-content-sha256:${payload_hash}
x-amz-date:${amzdate}
"
  local signed_headers="host;x-amz-content-sha256;x-amz-date"
  local canonical_request="PUT
/${BUCKET}/${KEY}

${canonical_headers}
${signed_headers}
${payload_hash}"
  local cr_hash=$(sha256 "$canonical_request")
  local string_to_sign="AWS4-HMAC-SHA256
${amzdate}
${scope}
${cr_hash}"
  local kDate=$(printf '%s' "$datestamp" | hmac "key:AWS4${SECRET}" | hex)
  local kRegion=$(printf '%s' "$REGION"  | hmac "hexkey:${kDate}"  | hex)
  local kService=$(printf '%s' "$SERVICE" | hmac "hexkey:${kRegion}" | hex)
  local kSigning=$(printf '%s' "aws4_request" | hmac "hexkey:${kService}" | hex)
  local signature=$(printf '%s' "$string_to_sign" | hmac "hexkey:${kSigning}" | hex)
  local auth="AWS4-HMAC-SHA256 Credential=${ACCESS}/${scope}, SignedHeaders=${signed_headers}, Signature=${signature}"
  curl -sS -o /dev/null -w '%{http_code}' -X PUT "${endpoint}/${BUCKET}/${KEY}" \
    -H "Host: ${host}" \
    -H "X-Amz-Content-Sha256: ${payload_hash}" \
    -H "X-Amz-Date: ${amzdate}" \
    -H "Authorization: ${auth}" \
    --data-binary "$BODY"
}

echo "── seeding object via direct PUT (localhost:19000, no CF) ──"
echo "seed status: $(seed_put 'localhost:19000' 'http://localhost:19000')"
echo "key: $KEY"

# ─── Presigned GET, host = minio.srclode.online, tried at both paths ──
GET_URL_CF=$(presign GET  'minio.srclode.online' "$KEY" 600 "" https)
GET_URL_DR=$(presign GET  'localhost:19000'      "$KEY" 600 "" http)

echo
echo "── presigned GET, signed for host=minio.srclode.online ──"
echo "  → via CF:      $(curl -sS -o /dev/null -w '%{http_code}' "$GET_URL_CF")"
# For direct, the SIGNED host is minio.srclode.online but we hit
# localhost:19000. Because Host is verified, expect 403.
echo "  → direct:      $(curl -sS -o /dev/null -w '%{http_code}' -H 'Host: minio.srclode.online' "$GET_URL_DR")"

echo
echo "── presigned GET, signed for host=localhost:19000 ──"
GET_URL_DR2=$(presign GET  'localhost:19000'      "$KEY" 600 "" http)
echo "  → direct:      $(curl -sS -o /dev/null -w '%{http_code}' "$GET_URL_DR2")"

echo
echo "── presigned PUT, signed for host=minio.srclode.online ──"
PUT_URL_CF=$(presign PUT  'minio.srclode.online' "${KEY%.txt}-put.txt" 600 "" https)
# The one-shot: does a presigned PUT survive CF? SDK signs only host in
# the presigned form; CF may rewrite Accept-Encoding but the signature
# doesn't cover it, so validation should pass.
echo "  → via CF (aws-cli default AE — could be gzip or nothing): $(curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary "$BODY" "$PUT_URL_CF")"
echo "  → via CF (Accept-Encoding: gzip, br explicit):            $(curl -sS -o /dev/null -w '%{http_code}' -X PUT -H 'Accept-Encoding: gzip, br' --data-binary "$BODY" "$PUT_URL_CF")"

# Also try presigned PUT with a signed-headers set that INCLUDES
# Accept-Encoding. This would fail via CF if the SDK ever generates
# presigned URLs that way. Does aws-sdk-go-v2 ever do this? Very rarely.
# But test the failure shape for completeness.
echo
echo "── presigned PUT with SignedHeaders=host;accept-encoding (unusual) ──"
KEY2="${KEY%.txt}-put2.txt"
presign_ae() {
  local method=$1 host=$2 key=$3 expires=$4
  local amzdate=$(date -u +%Y%m%dT%H%M%SZ)
  local datestamp=$(date -u +%Y%m%d)
  local payload_hash="UNSIGNED-PAYLOAD"
  local scope="${datestamp}/${REGION}/${SERVICE}/aws4_request"
  local canonical_uri="/${BUCKET}/${key}"
  local q_credential=$(urlenc "${ACCESS}/${scope}")
  local q_signedheaders="accept-encoding;host"
  local canonical_query="X-Amz-Algorithm=AWS4-HMAC-SHA256"
  canonical_query+="&X-Amz-Credential=${q_credential}"
  canonical_query+="&X-Amz-Date=${amzdate}"
  canonical_query+="&X-Amz-Expires=${expires}"
  canonical_query+="&X-Amz-SignedHeaders=$(urlenc "$q_signedheaders")"
  local canonical_headers="accept-encoding:identity
host:${host}
"
  local canonical_request="${method}
${canonical_uri}
${canonical_query}
${canonical_headers}
${q_signedheaders}
${payload_hash}"
  local cr_hash=$(sha256 "$canonical_request")
  local string_to_sign="AWS4-HMAC-SHA256
${amzdate}
${scope}
${cr_hash}"
  local kDate=$(printf '%s' "$datestamp" | hmac "key:AWS4${SECRET}" | hex)
  local kRegion=$(printf '%s' "$REGION"  | hmac "hexkey:${kDate}"  | hex)
  local kService=$(printf '%s' "$SERVICE" | hmac "hexkey:${kRegion}" | hex)
  local kSigning=$(printf '%s' "aws4_request" | hmac "hexkey:${kService}" | hex)
  local signature=$(printf '%s' "$string_to_sign" | hmac "hexkey:${kSigning}" | hex)
  printf 'https://%s%s?%s&X-Amz-Signature=%s' "$host" "$canonical_uri" "$canonical_query" "$signature"
}
URL2=$(presign_ae PUT 'minio.srclode.online' "$KEY2" 600)
# Client sends AE=identity (as signed); CF rewrites to gzip,br;
# MinIO validates AE against gzip,br ≠ identity → 403.
echo "  → via CF (AE signed as identity, wire gets CF-rewritten):"
echo "     $(curl -sS -X PUT --data-binary "$BODY" -H 'Accept-Encoding: identity' -o /dev/null -w '%{http_code}' "$URL2")"
