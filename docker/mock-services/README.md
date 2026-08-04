# mock-services — unified local docker-deployer

A single Go service that stands in for the platform's whole deploy/runtime
plane on a local machine, backed by the **host docker daemon**. One HTTP server
fronts three self-identifying API groups:

| Group | Prefix | Stands in for | Used by |
|-------|--------|---------------|---------|
| **aws** | `/` (+ `/aws`) | the EC2 API | brahmi's aramb-vm provider (`AWS_ENDPOINT_URL_EC2`) |
| **narnia** | `/narnia/*` | narnia + narnia-workers (the k8s deployer) | jumbo (`NARNIA_BASE_URL`) |
| **baghira** | `/baghira/*` | baghira (pod status) | pool-manager (`BAGHIRA_BASE_URL`) |

Together they let the local stack run the full **jumbo → narnia → baghira**
deploy path — agent pool warm, brahmi scale up/down, and normal service
deploys — without running any real k8s (no narnia, narnia-workers, k3s/argocd,
baghira, or baghira-proxy). **jumbo stays the sole book-keeper; this binary is
the "cluster".**

Plus the original job: translating brahmi's EC2 calls into docker operations.

---

## Why

In the local `clode-stack`, after brahmi claims an agent it asks jumbo to
scale it (a normal jumbo deployment). jumbo writes a `deployments` row
(default status `accepted`) and POSTs a batch to `NARNIA_BASE_URL`. With no
narnia running, that POST fails and the row is stuck at `accepted` forever —
nothing ever calls jumbo back to advance it. Standing up the real k8s chain
(5 services + argocd) just to unstick this is far too heavy for a laptop.

This deployer replaces that chain with one always-up container that speaks the
same contracts.

---

## How a deploy flows

The narnia batch jumbo sends is **metadata only** — no image/env/ports. The
deployer pulls the real config back from jumbo, then acts:

```
jumbo ──POST /narnia/internal/deployments/batch──▶ mock-services (201, async)
                                                      │
        ◀──GET /internal/deployments/:id/config────── │   (image, vars, secrets, regions[0].replicas)
                                                      │
                                          replicas 0 → docker rm  |  ≥1 → docker run
                                                      │
        ◀──PUT /internal/deployments/:id/status────── │   status=completed + PRIVATE_URL
```

`status=completed` is what moves the deployment out of `accepted`; jumbo then
persists the outputs and bumps the service version.

### k8s-derivable DNS, faked with a docker alias

In-cluster, consumers dial a service at `{slug}-backend-main.{slug}.svc:{port}`
(e.g. ikki's browser CDP probe). Each deployed container is therefore given
docker **network aliases** so that exact name resolves on the shared `clode`
bridge — no consumer code changes:

- `{slug}-backend-main.{slug}.svc`  — the in-cluster service DNS name
- `{slug}.clode.internal`           — the private host jumbo stores in `PRIVATE_URL`

### One container per service — three docker primitives

| Concern | Primitive | Value |
|---------|-----------|-------|
| human / DNS name | container **name** | the service **slug** |
| id lookup (baghira) | label `aws.mock.service-id` | jumbo service **uuid** |
| ownership (sweep) | label `aws.mock.deployed-service=true` | — |
| k8s DNS | network **aliases** | `{slug}-backend-main.{slug}.svc`, `{slug}.clode.internal` |

baghira's `?serviceIdentifier=<uuid>&idType=id` is then a one-line label query
— no in-memory registry, and it survives a restart because **docker is the
store**.

---

## API

### aws group — EC2 wire protocol → docker

`POST /` (form-urlencoded `Action=…`), responses in EC2 XML. Signatures are
ignored (localhost, no auth surface).

| Action | docker |
|--------|--------|
| `RunInstances` | `docker create` + `start` (named `i-<hex>`) |
| `StopInstances` | `docker stop` (or `pause` when `Hibernate=true`) |
| `StartInstances` | `docker start` (or `unpause`) |
| `TerminateInstances` | `docker rm -f` |
| `RebootInstances` | `docker restart` |
| `DescribeInstances` | live state, honoring `InstanceId.N` + `Filter.N` |
| `DescribeSubnets` / `DescribeSecurityGroups` | empty set (leave the brahmi selectors unset) |

Plus an out-of-band JSON control plane:

- `PUT /_admin/config/default-image` `{"image":"<ref>"}` — image `RunInstances` launches
- `GET /_admin/config/default-image` — readback (clode-stack's sweep scripts read this)
- `GET /_admin/config` — full admin config

### narnia group — deployer facade

- `POST /narnia/internal/deployments/batch` — acks `201` immediately, then per
  deployment (async): pull config → `replicas==0` stop / `≥1` run with the DNS
  aliases → status callback (`completed` + `PRIVATE_URL`, or `failed`).
- `POST /narnia/internal/deletion-jobs` and `/bulk` — stop/remove by service id.

### baghira group — pod status

- `GET /baghira/api/v1/replicas?serviceIdentifier=<uuid>&idType=id` →
  `{"status":"SUCCESS","data":[{"status":"Running","ready":"1/1", …}]}`.
  Empty `data` when the service isn't deployed (pool-manager reads that as
  "not healthy yet").

### Liveness

- `GET /health` → `{"status":"ok"}`

---

## Layout

```
cmd/mock-services/main.go     minimal entrypoint: config → build groups → serve
internal/
  config/               flags + env → Config
  server/               the Fiber app: route groups + per-group scoped logging
  deploy/               shared docker service-deployer (run/stop/replicas by label)
  mock/                 the services this binary IMPERSONATES (inbound APIs)
    aws/                the EC2-to-docker engine (mounted via net/http adaptor)
    narnia/             deploy / delete / status-callback handlers
    baghira/            replicas handler
  client/               the services this binary CALLS (outbound)
    jumbo/              tiny client: GET config, PUT status
```

`mock/*` is what we answer *as*; `client/*` is what we reach *out to*. `deploy`
is the shared docker engine the mocks place containers through.

Logs are group-scoped, e.g.:

```
[aws] POST / -> 200 (10ms)
[narnia] deployed smoke-svc (service=… image=nginx:alpine port=80)
[baghira] GET /baghira/api/v1/replicas?… -> 200 (3ms)
```

---

## Configuration

Flags (with env fallbacks):

| Flag | Env | Default | Purpose |
|------|-----|---------|---------|
| `--addr` | `MOCK_SERVICES_ADDR` | `:8080` | listen address |
| `--network` | — | `bridge` | docker network launched containers attach to |
| `--pull-policy` | — | `IfNotPresent` | `IfNotPresent` \| `Always` \| `Never` |
| `--docker-socket` | — | (DOCKER_HOST / `/var/run/docker.sock`) | daemon socket |
| `--entrypoint-override` | — | — | replace the image entrypoint on aws launches |
| `--jumbo-base-url` | `JUMBO_BASE_URL` | `http://jumbo:8080` | jumbo, for narnia config-pull + status |

`--pull-policy Never` is the right choice when images live under a local-only
namespace (`clode-stack/*`) — a pull would hit Docker Hub and 401.

---

## In clode-stack

`mock-services` is **always up** (no compose profile). Wiring in `docker-compose.yml`:

- `mock-services`: `command: [--network clode --pull-policy Never]`, `JUMBO_BASE_URL=http://jumbo:8080`, host docker socket bind-mounted.
- `jumbo`: `NARNIA_BASE_URL=${NARNIA_BASE_URL:-http://mock-services:8080/narnia}` (override to `http://narnia:8081` for `--profile deploy` real narnia).
- `pool-manager`: `LOCAL_MODE=false`, `BAGHIRA_BASE_URL=http://mock-services:8080/baghira`.
- `brahmi`: `AWS_ENDPOINT_URL_EC2=http://mock-services:8080` (aramb-vm path).

`scripts/seed.sh` PUTs the kairo image to `/_admin/config/default-image`, and
`scripts/lib/agent-sweep.sh` reclaims every container labelled `aws.mock.*`
(including `aws.mock.deployed-service`).

---

## Build, run, test

```bash
make build            # -> bin/mock-services
make run              # build + run (:8080)
make test             # unit + e2e (e2e needs a reachable docker daemon)
make test-unit        # MOCK_SERVICES_SKIP_E2E=1 go test ./...
make test-e2e         # full Run/Stop/Start/Hibernate/Terminate against real docker
make vet
```

### Standalone (outside clode-stack)

```bash
docker network create demo
./bin/mock-services --addr :18099 --network demo --jumbo-base-url http://127.0.0.1:8080

# aws
aws --endpoint-url http://localhost:18099 ec2 describe-instances

# narnia (jumbo must serve /internal/deployments/:id/config + /status)
curl -X POST localhost:18099/narnia/internal/deployments/batch \
  -d '{"batch_id":"b1","deployments":[{"deployment_id":"<id>"}]}'

# baghira
curl 'localhost:18099/baghira/api/v1/replicas?serviceIdentifier=<uuid>&idType=id'
```
