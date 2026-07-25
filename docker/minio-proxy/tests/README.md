# minio-proxy — SigV4 regression tests

Hand-rolled openssl-based SigV4 signers that reproduce the truth table
this proxy exists to fix. Zero language dependencies — bash + openssl +
curl. Any of these can be run at any time to prove the CF-fronted path
is (or isn't) still working.

Each script is self-contained and idempotent (uploads test objects with
`repro/` prefix; the `cleanup.sh --minio` flag from the stack root wipes
them).

## When to run which

| Script | What it proves | Run when |
|---|---|---|
| `sigv4-put.sh` | Baseline: signed PUT `AE=identity` succeeds through both direct and CF-fronted paths (thanks to the proxy) | You suspect the CF path is broken. If direct=200 but CF=403, the proxy is failing to rewrite Accept-Encoding — check `docker logs clode-minio-proxy-1` and hit `/_proxy/echo` via CF. |
| `sigv4-put-b.sh` | Isomorphic mirror: signing `AE=gzip, br` succeeds via CF (because CF's rewrite matches what you signed) and fails direct. Combined with `sigv4-put.sh`, forms the two-cell truth table that pinned the bug to Accept-Encoding. | Sanity check that CF is still doing the rewrite it always did. If CF starts BEHAVING and passes `identity` through unchanged, this script will start failing via CF — that would be the signal to remove the proxy. |
| `sigv4-put-c.sh` | Signing with `Accept-Encoding` NOT in the signed-headers set passes both paths. Proves AE is the ONLY CF-mangled header — Host / X-Amz-Date / X-Amz-Content-Sha256 / body all survive. | You suspect CF has started mangling a NEW header. If this script starts failing via CF, hit `/_proxy/echo` on the CF host and diff against a known-good baseline to find what else CF is now rewriting. |
| `presign-test.sh` | Presigned URLs (both GET and PUT) work through CF WITHOUT the proxy — because SDK presigners only sign `host`. | You're evaluating whether the two-endpoint refactor (in-network wire + public presigned URLs) is viable. |

## Preconditions

- clode-stack is up (`./stack.sh up`), which means:
  - `clode-minio-1` is healthy → gives you `localhost:19000` (host publish)
  - `clode-cloudflared-1` is healthy → `minio.srclode.online` reaches your tunnel
  - `clode-minio-proxy-1` is healthy → serves the CF ingress target
- MinIO admin creds are the compose defaults (`minioadmin`/`minioadmin`)
- The `brahmi-attachments` bucket exists (minio-setup creates it on first
  boot). If missing, run `./stack.sh seed` or bring the stack up cold.

## Running

```
cd docker/minio-proxy/tests/
./sigv4-put.sh          # ← start here
./sigv4-put-b.sh
./sigv4-put-c.sh
./presign-test.sh
```

Each prints a header per test cell, the HTTP status code, and (on
failure) MinIO's error body. Every 200 is a pass; every 403 is an
expected failure per the script's comment header.

## Interpreting a fresh regression

`sigv4-put.sh` failing at the CF cell is the canonical "the proxy is
gone or the fix regressed" signal. Steps:

1. `curl -I https://minio.srclode.online/` — is `X-Proxied-By:
   minio-proxy/1.0` present? If not, cloudflared isn't routing to the
   proxy; check `cloudflared-config.yml`.
2. `curl https://minio.srclode.online/_proxy/health/full` — 200 means
   nginx and minio are both alive and DNS between them is working.
3. `curl -H "Accept-Encoding: identity"
   https://minio.srclode.online/_proxy/echo` — the returned
   `accept-encoding:` line under "request as received" should be
   `gzip, br` (CF rewrote it). The rewrite hasn't happened yet at that
   point; it's what the proxy is about to do.
4. `docker logs --tail 20 clode-minio-proxy-1` — access log lines show
   `ae_in=... ae_out="identity"` for every request. If `ae_out` isn't
   `identity`, the nginx.conf's `proxy_set_header Accept-Encoding
   "identity"` is missing or in the wrong location block.
