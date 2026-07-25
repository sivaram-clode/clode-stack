# Cloudflared tunnel setup

`cloudflared-config.yml` checked into this repo is wired for the existing
clode-stack tunnel (`cf6cb58d-…` on `srclode.online`). If you're using
that tunnel and have the creds JSON in `~/.cloudflared/`, you don't need
this doc — `./stack.sh up` already does the right thing.

Use this doc only when:
- bringing up the stack against a **new Cloudflare account / domain**, or
- the existing tunnel was deleted and needs to be recreated.

## Prereqs

- A Cloudflare account.
- A domain whose nameservers point at Cloudflare (the domain shows up
  under "Websites" in the dash). Public hostnames will be subdomains of
  this — e.g. `raksha.your-domain.com`, `notify.your-domain.com`, etc.
- `cloudflared` CLI installed locally:
  ```bash
  # macOS: brew install cloudflared
  # debian/ubuntu: see https://pkg.cloudflare.com/
  cloudflared --version
  ```

## 1. Authenticate

```bash
cloudflared tunnel login
```
Opens the browser. Pick the zone (your domain). Writes
`~/.cloudflared/cert.pem` — this is the **origin cert**, used to manage
tunnels on that account. Keep out of git.

## 2. List existing tunnels (optional)

```bash
cloudflared tunnel list
```
Shows each tunnel's UUID + name. Skip if you know you need a fresh one.

## 3. Create a tunnel

```bash
cloudflared tunnel create clode-stack
```
Prints a UUID — note it down. Writes
`~/.cloudflared/<UUID>.json` (the tunnel **credentials** — keep out of
git).

## 4. Create a DNS record per service

One CNAME per hostname you'll expose, all pointing at the tunnel:
```bash
cloudflared tunnel route dns <UUID> raksha.your-domain.com
cloudflared tunnel route dns <UUID> jumbo.your-domain.com
cloudflared tunnel route dns <UUID> brahmi.your-domain.com
# … one per service listed in cloudflared-config.yml …
```

Idempotent — running twice reports "record exists" without breaking
anything. Add `--overwrite-dns` only if you're re-pointing an existing
CNAME (e.g. moving a hostname from an old tunnel to this one).

Verify each with `dig <svc>.your-domain.com CNAME +short` — should show
`<UUID>.cfargotunnel.com`.

Why explicit records instead of a wildcard: DNS is self-documenting
(anyone inspecting the zone can see which hostnames the tunnel serves),
routing failures are localised (typo in one record doesn't affect the
others), and CF's security-center findings map cleanly to real
subdomains instead of a wildcard blob.

## 5. Edit `cloudflared-config.yml`

In the repo root (`clode-stack/cloudflared-config.yml`):

- Replace the `tunnel:` value with your `<UUID>`.
- Replace the `credentials-file:` filename with `<UUID>.json`.
- Replace every `srclode.online` hostname with `your-domain.com`.
- Leave the `service: http://<svc>:<port>` targets alone — those are
  container DNS names on the `clode` bridge and don't depend on your
  domain.

Minimal shape:
```yaml
tunnel: <UUID>
credentials-file: /etc/cloudflared/<UUID>.json

ingress:
  - hostname: raksha.your-domain.com
    service: http://raksha:8080
  # … one entry per service that needs a public hostname …
  - service: http_status:404      # required fallback
```

Every `hostname:` here must have a matching CNAME from step 4.

## 6. Bring up the stack

```bash
./stack.sh up
```
The cloudflared service mounts `~/.cloudflared/` for creds and overlays
the repo's `cloudflared-config.yml` over `/etc/cloudflared/clode-stack-config.yml`.
Logs:
```bash
docker compose logs -f cloudflared
```
Look for `Registered tunnel connection` lines — usually 4 (one per
Cloudflare edge POP). If you see "tunnel <UUID> not found", the creds
JSON in `~/.cloudflared/` is for a different account than the one
`cert.pem` was issued for — re-run step 1.

## Adding a new public hostname later

Two steps, both required:

1. Add the ingress rule to `cloudflared-config.yml` above the
   `http_status:404` fallback:
   ```yaml
   - hostname: <svc>.your-domain.com
     service: http://<svc>:8080
   ```
2. Create the CNAME from the local `cloudflared` CLI (uses
   `~/.cloudflared/cert.pem` from step 1):
   ```bash
   cloudflared tunnel route dns <TUNNEL_UUID> <svc>.your-domain.com
   ```

Then `docker compose restart cloudflared` so the tunnel picks up the new
ingress rule.

For the current tunnel + zone:
```bash
cloudflared tunnel route dns cf6cb58d-b43b-4e84-9c68-cf14652c9fc4 <svc>.srclode.online
```

If your local CLI isn't set up (working on a machine that never ran
`cloudflared tunnel login`), you can borrow the container's cert instead:
```bash
docker exec clode-cloudflared-1 cloudflared tunnel \
  --origincert /secrets/cert.pem \
  route dns <TUNNEL_UUID> <svc>.your-domain.com
```
Same effect — the container mount and your `~/.cloudflared/cert.pem`
point at the same origin cert.

## Files involved

| Path | Role | In git? |
|---|---|---|
| `clode-stack/cloudflared-config.yml`           | tunnel ingress rules                                | yes |
| `~/.cloudflared/cert.pem`                      | origin cert (account-wide; lets cloudflared manage tunnels) | NO — secret |
| `~/.cloudflared/<tunnel-id>.json`              | tunnel credentials (per-tunnel)                     | NO — secret |
| compose mount `~/.cloudflared → /etc/cloudflared:ro` | makes both creds files visible to the container | n/a |
| compose mount `./cloudflared-config.yml → /etc/cloudflared/clode-stack-config.yml:ro` | overlays the repo config on top of the dir mount | n/a |
