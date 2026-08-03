#!/usr/bin/env python3
# Bind-mounted into the narnia-workers container as its command wrapper for
# the local-stack `deploy` profile. Waits for the in-cluster ArgoCD (from the
# `k3s` compose service) to answer, logs in as admin with the local-only
# password preseeded by init-argocd.sh, mints a long-lived apiKey token,
# exports ARGOCD_ADDRESS/API_KEY/PROJECT, then execs the worker binary.
#
# Runs inside the narnia-workers image without any Dockerfile edit — the
# base image already ships python3, which covers HTTP + JSON parsing.
# (stdlib only — stacklib is not mounted into the container.)
import json
import os
import re
import ssl
import sys
import time
import urllib.error
import urllib.request

ARGOCD_HOST = os.environ.get("ARGOCD_HOST") or "k3s:30443"
ARGOCD_ADMIN_USER = os.environ.get("ARGOCD_ADMIN_USER") or "admin"
ARGOCD_ADMIN_PW = os.environ.get("ARGOCD_ADMIN_PW") or "clode-local"

# -k / insecure: skip TLS verification (self-signed in-cluster cert).
_INSECURE = ssl.create_default_context()
_INSECURE.check_hostname = False
_INSECURE.verify_mode = ssl.CERT_NONE


def log(*parts, err=False):
    msg = "[argocd-token] " + " ".join(str(p) for p in parts)
    print(msg, file=sys.stderr if err else sys.stdout, flush=True)


def extract(body, key):
    # Extract a single top-level key from JSON text. Returns empty string on
    # missing key or parse failure — caller checks emptiness.
    try:
        v = json.loads(body).get(key)
        if v:
            return v
    except Exception:
        pass
    return ""


def http_status(url):
    # GET url (insecure), return the numeric HTTP status. 000 on any
    # connection/transport failure — mirrors `curl -w '%{http_code}'`.
    try:
        req = urllib.request.Request(url, method="GET")
        with urllib.request.urlopen(req, context=_INSECURE, timeout=5) as r:
            return r.getcode()
    except urllib.error.HTTPError as e:
        return e.code
    except Exception:
        return 0


def http_post(url, payload, headers):
    # POST JSON string (insecure), return the response body text (including
    # error-response bodies, like `curl -s` piping the body onward).
    data = payload.encode()
    req = urllib.request.Request(url, data=data, method="POST")
    for k, v in headers.items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, context=_INSECURE) as r:
            return r.read().decode()
    except urllib.error.HTTPError as e:
        try:
            return e.read().decode()
        except Exception:
            return ""
    except Exception:
        return ""


def main():
    log("waiting for argocd at https://%s" % ARGOCD_HOST)
    i = 0
    while not re.match(r'^(2..|4..)$', str(http_status(f"https://{ARGOCD_HOST}/api/v1/session"))):
        i += 1
        if i > 150:
            log(f"argocd never came up at https://{ARGOCD_HOST}", err=True)
            sys.exit(1)
        time.sleep(2)
    log("argocd reachable")

    log("logging in as admin")
    session = extract(
        http_post(
            f"https://{ARGOCD_HOST}/api/v1/session",
            json.dumps({"username": ARGOCD_ADMIN_USER, "password": ARGOCD_ADMIN_PW}),
            {"Content-Type": "application/json"},
        ),
        "token",
    )
    if not session:
        log("session login failed (is admin password preseeded?)", err=True)
        sys.exit(1)

    log("minting long-lived apiKey token")
    token = extract(
        http_post(
            f"https://{ARGOCD_HOST}/api/v1/account/{ARGOCD_ADMIN_USER}/token",
            json.dumps({"name": "narnia-workers", "expiresIn": "0"}),
            {"Authorization": f"Bearer {session}", "Content-Type": "application/json"},
        ),
        "token",
    )
    if not token:
        log("apiKey token mint failed (is accounts.admin=apiKey,login patched?)", err=True)
        sys.exit(1)

    os.environ["ARGOCD_ADDRESS"] = ARGOCD_HOST
    os.environ["ARGOCD_API_KEY"] = token
    os.environ["ARGOCD_PROJECT"] = os.environ.get("ARGOCD_PROJECT") or "default"

    log("argocd wired: ARGOCD_ADDRESS=%s ARGOCD_PROJECT=%s"
        % (os.environ["ARGOCD_ADDRESS"], os.environ["ARGOCD_PROJECT"]))
    os.execvp(sys.argv[1], sys.argv[1:])


if __name__ == "__main__":
    main()
