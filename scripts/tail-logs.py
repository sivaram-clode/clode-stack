#!/usr/bin/env python3
# tail-logs.py — start one background `docker compose logs -f` tailer per
# service, writing to logs/service/<svc>/<UTC-timestamp>.log. Called from
# up.sh after the stack is up. Each new ./up.sh creates a fresh timestamped
# file; previous logs are kept untouched on disk.
#
# Usage:
#   ./tail-logs.py                  # tail every service in the compose file
#   ./tail-logs.py jumbo brahmi     # tail only the listed services
#
# Tailers run as nohup'd background processes so they survive the parent
# shell exit. They exit on their own when the corresponding container is
# removed (docker compose down). down.py also explicitly reaps any stragglers.

import os
import sys
import signal
import subprocess
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s


def main():
    os.chdir(s.REPO_DIR)  # cd "$(dirname "$0")/.."

    LOGS_DIR = "logs/service"
    TS = s.utc_stamp()
    PID_FILE = f"{LOGS_DIR}/.tailer-pids"

    os.makedirs(LOGS_DIR, exist_ok=True)

    # If a previous run left tailers around, reap them first.
    if Path(PID_FILE).is_file():
        for line in Path(PID_FILE).read_text().splitlines():
            fields = line.split()
            if not fields:
                continue
            try:
                os.kill(int(fields[0]), signal.SIGTERM)
            except (ValueError, ProcessLookupError, PermissionError, OSError):
                pass
        os.remove(PID_FILE)

    Path(PID_FILE).touch()

    args = sys.argv[1:]
    if len(args) > 0:
        services = args
    else:
        out = s.compose_bare("config", "--services", capture=True).stdout
        services = out.split()

    count = 0
    with open(PID_FILE, "a") as pids:
        for svc in services:
            os.makedirs(f"{LOGS_DIR}/{svc}", exist_ok=True)
            out = f"{LOGS_DIR}/{svc}/{TS}.log"
            # --no-log-prefix strips the leading "svc-1  | " so the file reads like a
            # normal service log. -t adds RFC3339 timestamps per line.
            fout = open(out, "w")
            proc = subprocess.Popen(
                ["docker", "compose",
                 "-f", "docker-compose.yml", "-f", "docker-compose.cache.yml",
                 "logs", "--no-color", "--no-log-prefix", "-t", "-f", svc],
                stdout=fout, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
                start_new_session=True,
            )
            fout.close()
            pids.write(f"{proc.pid} {svc} {out}\n")
            count += 1

    s.log(f"tailing {count} services into ./{LOGS_DIR}/<svc>/{TS}.log")


if __name__ == "__main__":
    main()
