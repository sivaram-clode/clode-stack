# ec2-docker-mock

A minimal AWS EC2 API server that translates a small subset of the EC2 wire
protocol into `docker` operations against a bind-mounted daemon socket. Point
an `aws-sdk-go-v2` EC2 client at it — via `AWS_ENDPOINT_URL_EC2` — and
"launching an EC2 instance" becomes `docker create + start` on your host.

Written to let brahmi's `aramb-vm` provider run end-to-end locally without an
AWS account: **zero code changes on the brahmi side**, just env.

## Actions implemented

| EC2 action | Maps to | Notes |
|---|---|---|
| `RunInstances` | `docker create + start` | `AGENT_IMAGE` (parsed from cloud-init user-data) picks the actual image; `ImageId` is echoed back. A named docker volume is mounted at `/home/node/.benji` so `$BENJI_HOME` persists across stop/start. |
| `DescribeInstances` | `docker inspect` + tag filters | Honors `InstanceId.N`, `Filter.N.Name=tag:<k>` and `Filter.N.Name=instance-state-name`. |
| `StopInstances` | `docker stop` | With `Hibernate=true` → `docker pause` (cgroup-freezer analogue of EC2 hibernate). |
| `StartInstances` | `docker start` / `docker unpause` | Hibernated records take the unpause path. |
| `TerminateInstances` | `docker rm -f` | Volume is retained unless the caller passes `X-EC2Mock-RemoveVolume=true`. |
| `RebootInstances` | stop + start | |
| `CancelSpotInstanceRequests` | no-op success | Spot lifecycle isn't modeled. |
| `DescribeInstanceAttribute` | skeleton response | Enough for hibernation-eligibility probes. |
| `DescribeSubnets`, `DescribeSecurityGroups` | empty result set | Stubs — so a stray `AGENT_VM_SUBNET_SELECTOR` / `AGENT_VM_SG_SELECTOR` on the caller doesn't 501 the launch. |

Not implemented: everything else (VPC lifecycle, IAM, EBS, spot fleet, ELB…).
The mock accepts unrecognised actions with `HTTP 501 UnsupportedAction`.

Signatures are ignored — the mock trusts callers. Do not expose it outside a
private network.

## Run it

### With docker

```bash
docker build -t ec2-docker-mock .

docker run --rm -d \
  --name ec2mock \
  --network clode \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ec2-docker-mock \
  --addr :8080 \
  --network clode
```

- `-v /var/run/docker.sock:/var/run/docker.sock` — required. The mock talks to
  the host daemon to create/inspect the "instance" containers.
- `--network clode` (compose flag) — the mock's own container joins this bridge
  so callers on the same bridge can reach it as `ec2mock:8080`.
- `--network clode` (mock flag, distinct) — every launched "instance"
  container joins this bridge. Use the same value as above so brahmi can dial
  the returned `PrivateIP` directly.

Bind to the host if you want to hit it from outside compose:
`-p 4566:8080` and point clients at `http://localhost:4566`.

### From source

```bash
go build -o bin/ec2mock ./cmd/ec2mock
./bin/ec2mock --addr :4566 --network bridge
```

Flags:
- `--addr :8080` — listen address.
- `--docker-socket` — override the daemon socket path. Empty defers to
  `DOCKER_HOST` env, then `/var/run/docker.sock`.
- `--network bridge` — docker network launched containers attach to. Match
  this to the network the *caller* (e.g. brahmi) sits on so the returned IP
  is reachable.
- `--entrypoint-override` — path to a shim entrypoint. Use this for images
  whose baked entrypoint expects systemd/cloud-init to be present (the real
  aramb-vm AMI does). The shim can `source /etc/clode-agent/agent.env` and
  exec the intended binary.

## Wiring brahmi's aramb-vm provider

Set these on the brahmi service. Every one is a pre-existing knob — the mock
requires no brahmi code change.

| Var | Value | Purpose |
|---|---|---|
| `AWS_ENDPOINT_URL_EC2` | `http://ec2mock:8080` | Native `aws-sdk-go-v2` endpoint override (config ≥ v1.27, brahmi is on v1.32.7). |
| `AWS_ACCESS_KEY_ID` | any dummy (e.g. `test`) | SDK credential-chain must resolve to *something*; the mock never checks the signature. |
| `AWS_SECRET_ACCESS_KEY` | any dummy | Same. |
| `AWS_REGION` | `us-east-1` (any) | SDK region default. |
| `AGENT_VM_REGION` | `us-east-1` (any) | brahmi's own `New()` gate. |
| `AGENT_VM_CLODE_ENV` | `local` | Mandatory tagging isolation key. |
| `AGENT_VM_AMI_ID` | `ami-mock-000000` (any) | Cosmetic — the mock prefers `AGENT_IMAGE` from cloud-init user-data. Must be non-empty. |
| `AGENT_VM_SUBNET_SELECTOR` | *(unset / empty)* | Mock returns empty; setting a selector would match no subnet. |
| `AGENT_VM_SG_SELECTOR` | *(unset / empty)* | Same. |
| `AGENT_VM_INSTANCE_PROFILE` | *(unset)* | IAM is a no-op locally. |
| `AGENT_VM_SPOT_ENABLED` | `false` (recommended) | Avoids brahmi's spot-vs-hibernation interruption dance. `true` also works — the mock treats spot as a passthrough label. |
| `AGENT_PROVIDER` | `aramb-vm` (or org allow-list) | Routes cloud provision through the VM path. |

Pool `service_configurations.vars` still supplies `AGENT_IMAGE` — that's the
docker image the mock actually runs. For local, use whatever `benji` / kairo
image you have on the daemon (or one built from the aramb-vm bootstrap image
with the `--entrypoint-override` shim).

## What it does NOT do

- **No persistence across restart.** State is in-memory. On boot the mock
  rehydrates its record set from `docker ps -a --filter label=aws.mock.instance-id`
  (`Rehydrate()` in `state.go`), so containers survive a mock restart cleanly.
  It does *not* preserve VPC/subnet/SG lookup tables (there aren't any).
- **No signature validation.** Anyone who can reach the port can drive it.
- **No spot lifecycle.** `spotInstanceRequestId` is never emitted, so callers
  that dive into it (brahmi's `CancelSpotRequest` does) find nothing and
  silently no-op — matching the on-demand code path.
- **No `DescribeSubnets` / `DescribeSecurityGroups` beyond empty sets.**
  A tag-selector that expects a match will fail on the *caller* side (brahmi
  errors "no subnet matched selector"); leave the selectors unset.

## Testing

```bash
go test ./...

# Full docker-daemon lifecycle test (skipped if docker isn't reachable):
go test ./internal/mock/ -run TestE2E_LifecycleAgainstRealDocker -v
```

Set `EC2MOCK_SKIP_E2E=1` to force-skip the docker-touching test in CI.
