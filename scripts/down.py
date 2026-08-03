#!/usr/bin/env python3
# clode-stack/down.py — stop the stack. Preserves everything.
#
# What this does:
#   - COMPOSE_PROFILES=<every profile> docker compose down --remove-orphans
#     so services tied to `profiles: [...]` (deploy, inbox, …) get stopped
#     too — compose ignores them otherwise.
#   - Reap per-service log tailers started by up.sh.
#   - Prune each ./logs/service/<svc>/ dir to the newest 10 files.
#
# What this does NOT do:
#   - Touch volumes, images, buildkit cache, or agent containers.
#     For those, use `./stack.sh wipe`. That separation is deliberate —
#     `down` is a fast reversible stop; `wipe` is the destructive path
#     that reads --yes and asks for confirmation.

import os
import sys
import signal
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s


def main():
    os.chdir(s.REPO_DIR)  # cd "$(dirname "$0")/.."

    # down.py takes no flags. Anything passed is a user misunderstanding —
    # most likely `--wipe` from muscle memory. Point them at the right script.
    args = sys.argv[1:]
    if len(args) > 0:
        print(f"down.py: unexpected argument(s): {' '.join(args)}", file=sys.stderr)
        print("        for a destructive teardown use: ./stack.sh wipe", file=sys.stderr)
        sys.exit(2)

    # Include every profile so services tied to `profiles: [...]` (deploy, inbox, …)
    # get stopped/removed too — `docker compose down` ignores them otherwise.
    profiles_out = s.run(
        ["docker", "compose", "config", "--profiles"], capture=True
    ).stdout
    compose_profiles = ",".join(profiles_out.splitlines())

    # Reap any per-service log tailers started by up.sh.
    pid_file = Path("logs/service/.tailer-pids")
    if pid_file.is_file():
        for line in pid_file.read_text().splitlines():
            fields = line.split()
            if not fields:
                continue
            try:
                os.kill(int(fields[0]), signal.SIGTERM)
            except (ValueError, ProcessLookupError, PermissionError, OSError):
                pass
        pid_file.unlink()
        s.log("stopped log tailers (files in ./logs/service/<svc>/ preserved)")

    # Prune per-service run logs to the 10 most recent files each.
    logs_service = Path("logs/service")
    if logs_service.is_dir():
        for name in sorted(os.listdir(logs_service)):
            svc_dir = f"logs/service/{name}/"
            if not Path(svc_dir).is_dir():
                continue
            files = sorted(
                p.name
                for p in Path(svc_dir).iterdir()
                if p.is_file() and p.name.endswith(".log")
            )
            count = len(files)
            if count > 10:
                remove = count - 10
                s.log(f"pruning {remove} old log(s) from {svc_dir} (kept newest 10)")
                for i in range(remove):
                    try:
                        os.remove(f"{svc_dir}{files[i]}")
                    except OSError:
                        pass

    s.log(
        "docker compose down --remove-orphans   "
        "(preserves volumes — use `./stack.sh wipe` to drop)"
    )
    s.docker(
        "compose", "down", "--remove-orphans",
        env={"COMPOSE_PROFILES": compose_profiles},
    )


if __name__ == "__main__":
    main()
