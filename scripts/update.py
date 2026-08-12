#!/usr/bin/env python3
"""update — bring the stack's service checkouts to their latest `main`.

For every service in scope, resolve its git checkout (the build context each
compose service points at — `../<svc>` by default), switch it to `main`, and
fast-forward to the remote. Every repo is independent: a problem with one
(dirty tree, no `main`, diverged history, `main` held by another worktree) is
LOGGED and SKIPPED, never fatal — the run continues and finishes with a summary.

Scope (positional tokens are auto-classified; a token that names a compose
profile is a profile, otherwise it's a service name):

    ./update.py                     # every buildable service across all profiles
                                    #   + the agent/state sibling repos (benji, …)
    ./update.py browser             # `browser` profile: default services + ikki, louie
    ./update.py brahmi raksha       # just those services' repos
    ./update.py --profile voice,org # explicit profile flag (CSV or repeatable)

Repos are updated in parallel (the cost is network-bound `git fetch`, and each
checkout is independent); per-repo output is buffered and flushed in repo order,
so a parallel run reads like a sequential one. Ctrl+C stops cleanly — no
traceback, partial summary, not-yet-started repos left untouched.

Flags:
    -n, --dry-run     show what each repo would do; touch nothing
        --no-fetch    skip `git fetch` (use already-fetched remote refs)
    -j, --jobs N      max repos updated concurrently (default 8, or $UPDATE_JOBS)
        --branch B    target branch instead of `main`
        --remote R    remote to fast-forward from (default: the branch's
                      upstream remote, else `origin`)

Safety: a repo with uncommitted changes is SKIPPED (never stashed or reset),
and the fast-forward is `--ff-only`, so a diverged local `main` is reported and
skipped rather than merged. The stack's own repo is always excluded. Exit code
is 0 unless an unexpected internal error occurs — skips are an expected outcome,
not a failure.
"""
import os
import sys
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s  # noqa: E402

# Related sibling repos the stack builds outside compose (agent image, browser
# base, baked state) — included in a full `update` (no scope given) and matchable
# by name. Each maps a label to a path relative to the stack repo; the git
# top-level is what actually gets updated, so a sub-path like brave-headed folds
# into its parent repo automatically.
EXTRA_REPOS = {
    "benji": "../benji",
    "agent-base-docker": "../agent-base-docker",
    "benji-state": "../benji-state",
    "aramb-skills": "../aramb-skills",
}


def _git(repo, *args, capture=True, check=False):
    """git -C <repo> … — capture by default, non-fatal by default (callers decide)."""
    return s.run(["git", "-C", str(repo), *args], capture=capture, check=check)


def _svc_dirs(profiles, names):
    """Map {label: checkout dir} for the services in scope.

    Reads the fully-resolved compose config (build contexts absolutized) under
    the selected COMPOSE_PROFILES, preferring a service's `src` additional build
    context (console-web's SPA source) over its base context. In-repo contexts
    (mock-services, k3s under ./docker) are dropped by the git-toplevel filter
    later; only sibling checkouts survive.
    """
    cfg = s.compose_config().get("services", {})
    out = {}
    for name, m in cfg.items():
        b = m.get("build")
        if not b or b is True:
            continue
        ctx = b.get("context") if isinstance(b, dict) else None
        src = ((b.get("additional_contexts") if isinstance(b, dict) else None) or {}).get("src")
        d = src or ctx
        if d:
            out[name] = Path(d)
    if names:
        # Explicit service names: keep only those, plus any that name an EXTRA repo.
        picked = {n: out[n] for n in names if n in out}
        for n in names:
            if n in EXTRA_REPOS:
                picked[n] = s.REPO_DIR / EXTRA_REPOS[n]
            elif n not in out:
                s.warn(f"unknown service: {n} (not a buildable compose service or known repo)")
        return picked
    # No explicit names. When no profile filter either, this is a full update —
    # fold in the agent/state sibling repos the stack builds outside compose.
    if not profiles:
        for label, rel in EXTRA_REPOS.items():
            out.setdefault(label, s.REPO_DIR / rel)
    return out


def _resolve_repos(labeled):
    """Collapse {label: dir} to unique git repos: {toplevel: [labels]}.

    A dir that isn't inside a git repo, or that resolves to the stack's own repo,
    is dropped (with a note). Multiple services sharing a checkout collapse to one.
    """
    stack_top = _git(s.REPO_DIR, "rev-parse", "--show-toplevel").stdout.strip()
    repos = {}   # toplevel -> sorted list of labels
    for label, d in sorted(labeled.items()):
        if not d.exists():
            s.warn(f"{label}: checkout not found at {d} — skipping")
            continue
        r = _git(d, "rev-parse", "--show-toplevel")
        if r.returncode != 0:
            s.warn(f"{label}: {d} is not a git repo — skipping")
            continue
        top = r.stdout.strip()
        if top == stack_top:
            continue  # never self-update the stack repo mid-run
        repos.setdefault(top, []).append(label)
    return repos


def _upstream_remote(repo, branch, override):
    """Remote to pull from: explicit override, else the branch's configured
    upstream remote, else `origin`."""
    if override:
        return override
    r = _git(repo, "config", f"branch.{branch}.remote")
    return r.stdout.strip() if r.returncode == 0 and r.stdout.strip() else "origin"


def update_repo(top, labels, *, branch, remote_override, fetch, dry_run):
    """Switch one repo to `branch` and fast-forward it.

    Returns (status, msgs): status is updated | current | skip | fail, and msgs
    is a buffered list of (kind, text) log lines the caller emits. Output is
    buffered rather than printed directly so parallel workers never interleave —
    each repo's lines are flushed together, in repo order, by the main thread.
    """
    msgs = []
    def _warn(t): msgs.append(("warn", t))
    def _log(t): msgs.append(("log", t))

    name = ", ".join(labels)
    tag = f"{name} ({Path(top).name})" if Path(top).name not in labels else name

    # 1) Refuse to touch real work — never stash/reset a user's changes. Only
    #    TRACKED modifications (staged or unstaged) count: untracked files (e.g.
    #    nested git worktrees or tooling dirs a repo forgot to ignore) don't block
    #    a `git switch` or `--ff-only` — git aborts safely if an incoming tracked
    #    file would clobber one, and that abort is caught below as a skip.
    porcelain = _git(top, "status", "--porcelain", "--untracked-files=no").stdout.strip()
    if porcelain:
        n = len(porcelain.splitlines())
        _warn(f"{tag}: {n} uncommitted tracked change(s) — SKIP (commit/stash first)")
        return "skip", msgs

    # 2) The target branch must exist locally or on the remote.
    has_local = _git(top, "show-ref", "--verify", "--quiet", f"refs/heads/{branch}").returncode == 0
    remote = _upstream_remote(top, branch, remote_override)

    if fetch and not dry_run:
        fr = _git(top, "fetch", "--prune", remote)
        if fr.returncode != 0:
            _warn(f"{tag}: git fetch {remote} failed — SKIP\n{fr.stderr.strip()}")
            return "skip", msgs

    has_remote = _git(top, "show-ref", "--verify", "--quiet",
                      f"refs/remotes/{remote}/{branch}").returncode == 0
    if not has_local and not has_remote:
        _warn(f"{tag}: no `{branch}` branch locally or on {remote} — SKIP")
        return "skip", msgs

    cur = _git(top, "rev-parse", "--abbrev-ref", "HEAD").stdout.strip()

    if dry_run:
        action = "already on" if cur == branch else f"switch {cur} ->"
        _log(f"{tag}: [dry-run] {action} {branch}, fast-forward from {remote}/{branch}")
        return ("current" if cur == branch else "updated"), msgs

    # 3) Switch onto the branch if we aren't already there. `git switch` refuses
    #    if `branch` is checked out in another linked worktree — caught -> skip.
    if cur != branch:
        sw = _git(top, "switch", branch)
        if sw.returncode != 0:
            _warn(f"{tag}: cannot switch {cur} -> {branch} — SKIP\n{sw.stderr.strip()}")
            return "skip", msgs

    # 4) Fast-forward only. A diverged local branch can't FF -> report + skip
    #    (leaving the checkout on `branch`, unmerged), never a silent merge.
    before = _git(top, "rev-parse", "HEAD").stdout.strip()
    pull = _git(top, "merge", "--ff-only", f"{remote}/{branch}") if has_remote else None
    if pull is not None and pull.returncode != 0:
        _warn(f"{tag}: cannot fast-forward {branch} from {remote} — SKIP\n{pull.stderr.strip()}")
        return "skip", msgs
    after = _git(top, "rev-parse", "HEAD").stdout.strip()

    if cur != branch:
        _log(f"{tag}: switched {cur} -> {branch}"
             + (f", fast-forwarded to {after[:9]}" if after != before else f" (at {after[:9]})"))
        return "updated", msgs
    if after != before:
        _log(f"{tag}: {branch} fast-forwarded {before[:9]} -> {after[:9]}")
        return "updated", msgs
    _log(f"{tag}: already up to date ({after[:9]})")
    return "current", msgs


def parse_args(argv):
    profiles, names = [], []
    branch = "main"
    remote = ""
    fetch = True
    dry_run = False
    jobs = int(os.environ.get("UPDATE_JOBS") or 8)
    i, n = 0, len(argv)
    while i < n:
        a = argv[i]
        if a in ("-n", "--dry-run"):
            dry_run = True; i += 1
        elif a == "--no-fetch":
            fetch = False; i += 1
        elif a in ("-j", "--jobs"):
            if i + 1 >= n:
                s.die("--jobs requires a value")
            jobs = int(argv[i + 1]); i += 2
        elif a.startswith("--jobs="):
            jobs = int(a[len("--jobs="):]); i += 1
        elif a == "--branch":
            if i + 1 >= n:
                s.die("--branch requires a value")
            branch = argv[i + 1]; i += 2
        elif a.startswith("--branch="):
            branch = a[len("--branch="):]; i += 1
        elif a == "--remote":
            if i + 1 >= n:
                s.die("--remote requires a value")
            remote = argv[i + 1]; i += 2
        elif a.startswith("--remote="):
            remote = a[len("--remote="):]; i += 1
        elif a == "--profile":
            if i + 1 >= n:
                s.die("--profile requires a value")
            profiles.extend(argv[i + 1].split(",")); i += 2
        elif a.startswith("--profile="):
            profiles.extend(a[len("--profile="):].split(",")); i += 1
        elif a == "--":
            names.extend(argv[i + 1:]); break
        elif a.startswith("-"):
            s.die(f"unknown flag: {a}")
        else:
            names.append(a); i += 1
    if jobs < 1:
        s.die("--jobs must be >= 1")
    return profiles, names, branch, remote, fetch, dry_run, jobs


def _emit(msgs):
    """Flush a repo's buffered (kind, text) lines through the shared logger."""
    for kind, text in msgs:
        (s.warn if kind == "warn" else s.log)(text)


def main(argv=None):
    profiles, tokens, branch, remote, fetch, dry_run, jobs = parse_args(
        sys.argv[1:] if argv is None else argv)

    # Auto-classify bare positional tokens: a token that names a compose profile
    # is a profile; anything else is a service name.
    known = set(s.compose_profiles().split(",")) if s.compose_profiles() else set()
    names = []
    for t in tokens:
        (profiles if t in known else names).append(t)

    # Profiles decide which gated services compose reveals. With none selected we
    # enable them ALL, so a bare `update` (or a `update <svc>` for a gated svc)
    # sees every service; a profile filter narrows to that profile's active set.
    if profiles:
        os.environ["COMPOSE_PROFILES"] = ",".join(dict.fromkeys(profiles))
        s.log(f"scope: profiles = {os.environ['COMPOSE_PROFILES']}")
    else:
        os.environ["COMPOSE_PROFILES"] = s.compose_profiles()
    if names:
        s.log(f"scope: services = {' '.join(names)}")

    labeled = _svc_dirs(profiles, names)
    repos = _resolve_repos(labeled)
    if not repos:
        s.warn("no repositories in scope")
        return

    order = sorted(repos)
    # The work is network-bound `git fetch`, and each repo is an independent
    # checkout, so run them concurrently. Threads (not processes) are right:
    # every step shells out to git, and subprocess I/O releases the GIL. Each
    # repo's own git steps stay strictly sequential inside update_repo; only the
    # repos run in parallel. A pure dry-run without fetch does no network I/O, so
    # it stays single-threaded (deterministic, no pool overhead).
    workers = 1 if (dry_run and not fetch) else max(1, min(jobs, len(order)))
    s.log(f"target branch `{branch}` across {len(order)} repo(s)"
          + (f", {workers} parallel" if workers > 1 else "")
          + (" [dry-run]" if dry_run else ""))

    def work(top):
        labels = sorted(repos[top])
        try:
            return update_repo(top, labels, branch=branch, remote_override=remote,
                               fetch=fetch, dry_run=dry_run)
        except Exception as e:   # never let one repo abort the sweep
            return "fail", [("warn", f"{', '.join(labels)}: unexpected error — SKIP: {e}")]

    tally = {"updated": [], "current": [], "skip": [], "fail": []}
    interrupted = False
    ex = ThreadPoolExecutor(max_workers=workers)
    futs = {top: ex.submit(work, top) for top in order}
    try:
        # Collect in repo order so parallel output reads like the sequential run;
        # each repo's buffered lines flush together the moment its result lands.
        for top in order:
            status, msgs = futs[top].result()
            _emit(msgs)
            tally[status].append(", ".join(sorted(repos[top])))
        ex.shutdown(wait=True)
    except KeyboardInterrupt:
        # Ctrl+C: drop not-yet-started repos; in-flight git children take the same
        # SIGINT from the terminal and unwind on their own. No traceback, no wait.
        interrupted = True
        ex.shutdown(wait=False, cancel_futures=True)
        print()
        s.warn("interrupted — stopping; unreported repos were cancelled or left untouched")

    print()
    s.log("update summary" + (" (interrupted — partial)" if interrupted else ""))
    for key, verb in (("updated", "updated"), ("current", "already current"),
                      ("skip", "skipped"), ("fail", "errored")):
        if tally[key]:
            print(f"    {verb:16} ({len(tally[key])}): {'; '.join(sorted(tally[key]))}")
    if not any(tally.values()):
        print("    (nothing to do)")
    if interrupted:
        raise SystemExit(130)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        # A Ctrl+C outside the sweep loop (setup, compose config) — exit cleanly.
        s.warn("interrupted")
        raise SystemExit(130)
