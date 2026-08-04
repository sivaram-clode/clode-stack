"""scripts/lib/agent_sweep.py — shared agent + volume sweep helpers.

Imported by cleanup.py (--agents) and wipe.py. Every "agent" container in
this stack lives outside `docker compose`'s project graph, so `compose
down` can't reach them and they linger on the `clode` bridge:

  1. pool-manager LOCAL_MODE spawns kairo containers via the docker
     socket. No mock labels — matched by IMAGE, and by their `kairo-`
     NAME prefix as a fallback (a rebuild leaves them on a dangling image
     id / an alternate tag that the image match misses).
  2. mock-services spawns aramb-vm containers named `i-<hex>` for RunInstances.
     Every container carries an `aws.mock.instance-id` label; every
     backing named volume carries `aws.mock.owned=true`. Matched by
     LABEL — image-agnostic, so it survives an image tag change.

Consumers get three functions:

  agent_images()          -> returns deduped image list
                             (mock-services GET -> JSON -> $BENJI_IMAGE, in order).
  sweep_agent_containers(dry)
                          -> docker rm -f (containers-by-label U
                             containers-by-image on the clode network).
  sweep_agent_volumes(dry)
                          -> docker volume rm on every volume labeled
                             aws.mock.owned=true (mock-services's per-instance
                             $BENJI_HOME volumes). Pool-manager LOCAL_MODE
                             agents don't use named volumes so nothing to
                             collect from that side.

Every function accepts an optional `dry` arg — pass "1" to print what
WOULD run instead of doing it. Empty / "0" runs for real.
"""
import json
import os
from pathlib import Path

import stacklib as s

# Ports must match what compose publishes for mock-services (see the
# `ports:` block in docker-compose.yml — 8100 -> 8080).
MOCK_SERVICES_URL = os.environ.get("MOCK_SERVICES_URL", "http://mock-services.localhost:8080")

# Path from the clode-stack root.
KAIRO_CFG = os.environ.get("KAIRO_CFG", "data/pool-manager-svc-configs.json")

# All agents attach to this docker network. Both filters scope through it
# to avoid torching an unrelated container that happens to share a benji
# tag.
AGENT_NETWORK = os.environ.get("AGENT_NETWORK", "clode")

# Container label mock-services stamps on every instance it launches (mirrors
# containerLabelInstanceID in mock-services/internal/mock/aws/docker.go).
MOCK_SERVICES_INSTANCE_LABEL = os.environ.get("MOCK_SERVICES_INSTANCE_LABEL", "aws.mock.instance-id")

# Container label mock-services's /narnia group stamps on every service container it
# deploys (mirrors deploy.LabelDeployed in the mock-services deploy package).
MOCK_SERVICES_DEPLOYED_LABEL = os.environ.get("MOCK_SERVICES_DEPLOYED_LABEL", "aws.mock.deployed-service")

# Volume label mock-services stamps on every named volume it creates (mirrors
# labelValueTrue + "aws.mock.owned" in ensureVolume).
MOCK_SERVICES_VOLUME_LABEL = os.environ.get("MOCK_SERVICES_VOLUME_LABEL", "aws.mock.owned=true")


def _kairo_cfg_path() -> Path:
    """Resolve KAIRO_CFG relative to the repo root when it isn't absolute
    (bash callers `cd` to the repo root before sourcing; Python doesn't)."""
    p = Path(KAIRO_CFG)
    return p if p.is_absolute() else (s.REPO_DIR / p)


def _dedup(lines):
    """awk 'NF && !seen[$0]++' — drop blank lines, keep first occurrence."""
    out = []
    seen = set()
    for line in lines:
        if not line.strip():
            continue
        if line not in seen:
            seen.add(line)
            out.append(line)
    return out


def agent_images():
    """Emit the deduped set of docker image refs that back every
    agent-class container in this stack. Precedence (first hit wins,
    but all sources are unioned because different container populations
    come from different sources):

      1. mock-services GET /_admin/config/default-image  — the live image mock-services
         is currently launching (source of truth when mock-services is up).
      2. .configs[].settings.image in pool-manager-svc-configs.json — the
         images pool-manager LOCAL_MODE spawns; also seeds mock-services on boot.
      3. $BENJI_IMAGE — up.sh --agent exports this, overrides
         the JSON's image at seed time; kept in the union so we still match
         containers launched under it before the JSON was resyncd.
    """
    lines = []

    # Live mock-services lookup — non-fatal on connection error / non-JSON body /
    # unset value. Any failure is swallowed (mirrors curl --fail piping
    # nothing into jq on connect failure / non-2xx / non-JSON).
    data = s.get_json(f"{MOCK_SERVICES_URL}/_admin/config/default-image", timeout=2)
    val = data.get("default_image") if isinstance(data, dict) else None
    if val:
        lines.append(val)

    # .configs[].settings.image from the pool-manager svc-configs JSON.
    cfg = _kairo_cfg_path()
    if cfg.is_file():
        try:
            data = json.loads(cfg.read_text())
            for c in data.get("configs", []):
                img = (c.get("settings") or {}).get("image")
                if img:
                    lines.append(img)
        except Exception:
            pass

    if os.environ.get("BENJI_IMAGE"):
        lines.append(os.environ["BENJI_IMAGE"])
    if os.environ.get("BROWSER_IMAGE"):
        lines.append(os.environ["BROWSER_IMAGE"])

    # Every local benji / brave-head image tag on the daemon. Images are now
    # tagged by branch/workspace/main (never :latest), so enumerate whatever
    # tags actually exist rather than a fixed list — catches containers launched
    # under any tag (:main, :<branch>, or a legacy :latest/:dev/…).
    imgs = s.docker("images", "--format", "{{.Repository}}:{{.Tag}}",
                    "--filter", "reference=clode-stack/benji",
                    "--filter", "reference=clode-stack/brave-head",
                    capture=True, check=False).stdout
    for ln in imgs.split():
        if ln and not ln.endswith(":<none>"):
            lines.append(ln)

    return _dedup(lines)


def _agent_container_ids(images):
    """Print container ids that are either labeled by mock-services OR built from
    one of the passed images, AND attached to the clode network. The sets
    are unioned via `sort -u` because `docker ps --filter` semantics can't
    OR across filter TYPES (label OR ancestor) in a single call."""
    ids = []

    # Set A: containers mock-services owns (label-based, image-agnostic).
    ids += s.containers(
        f"label={MOCK_SERVICES_INSTANCE_LABEL}",
        f"network={AGENT_NETWORK}",
    )

    # Set A2: services deployed via mock-services's /narnia group (label-based,
    # image-agnostic). Separate `docker ps` because multiple `--filter label`
    # AND-combine; this OR-unions with set A via the final sort -u.
    ids += s.containers(
        f"label={MOCK_SERVICES_DEPLOYED_LABEL}",
        f"network={AGENT_NETWORK}",
    )

    # Set B: containers matching any pool-manager image (image-based).
    # Multiple `--filter ancestor=` are OR'd by docker; the network filter
    # AND-combines. Skip the call entirely if no images resolved.
    if images:
        filters = [f"network={AGENT_NETWORK}"]
        for img in images:
            filters.append(f"ancestor={img}")
        ids += s.containers(*filters)

    # Set C: pool-manager LOCAL_MODE agents matched by NAME on the clode
    # network. Their image tag is not stable — a rebuild leaves the running
    # container on a bare image ID (dangling `<none>`), and some agents run
    # `sivaclode/kairo:latest`; both break the ancestor match in set B. Every
    # pool-manager agent is named `kairo-*` (pool-manager's container-name
    # prefix), which survives any retag, so match on that. Scoped to the clode
    # network so a compose service never matches (those are `clode-<svc>-1`).
    ids += s.containers(
        "name=kairo-",
        f"network={AGENT_NETWORK}",
    )

    return sorted({x for x in ids if x.strip()})


def sweep_agent_containers(dry="0"):
    """Remove every agent container. Prints one indented line per removed
    container. Returns whether or not it removed anything."""
    images = agent_images()

    ids = _agent_container_ids(images)
    if not ids:
        return

    # Show what we're about to touch — the operator gets one shot to Ctrl-C
    # if this is scoped too broadly.
    r = s.docker(
        "ps", "-a",
        "--filter", "id=" + ",".join(ids),
        "--format", "    {{.Names}}  ({{.Image}})",
        capture=True, check=False,
    )
    print(r.stdout, end="")

    if str(dry) == "1":
        print(f"  \033[2m$\033[0m docker rm -fv  # {len(ids)} container(s)")
        return
    # -v takes each container's ANONYMOUS volumes with it (named volumes are
    # untouched — mock-services's are swept by label in sweep_agent_volumes). This
    # keeps a rebuild from orphaning per-container scratch volumes.
    s.docker("rm", "-fv", *ids, capture=True)


def sweep_agent_volumes(dry="0"):
    """Remove every named volume mock-services owns. Volume-remove is safe here
    because sweep_agent_containers is called first by every consumer — the
    volumes are detached by the time we get here. If a volume is still in
    use, docker volume rm returns non-zero; suppress it (best-effort)
    rather than aborting the wipe."""
    r = s.docker(
        "volume", "ls", "-q", "--filter", f"label={MOCK_SERVICES_VOLUME_LABEL}",
        capture=True, check=False,
    )
    vols = [v for v in r.stdout.splitlines() if v.strip()]
    if not vols:
        return

    for v in vols:
        print(f"    {v}")
    if str(dry) == "1":
        print(f"  \033[2m$\033[0m docker volume rm  # {len(vols)} volume(s)")
        return
    s.docker("volume", "rm", *vols, capture=True, check=False)
