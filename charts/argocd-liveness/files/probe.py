#!/usr/bin/env python3
"""One bounded, zero-LLM GitOps observation. Durable state lives in Kubernetes.

No Argo refresh/sync, git execution, repo code or operator credential is used.
Delivery is at least once: a crash after POST but before saving can duplicate it.
"""
import copy
import datetime
import json
import pathlib
import re
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


def iso(timestamp):
    return datetime.datetime.fromtimestamp(timestamp, datetime.timezone.utc).isoformat()


def assess(app, head, repo, branch, now, threshold):
    source = app.get("spec", {}).get("source", {})
    if (app.get("spec", {}).get("sources") or
            source.get("repoURL", "").removesuffix(".git") != "https://github.com/" + repo or
            source.get("targetRevision") != branch):
        return "unsupported or mismatched Application source (expected single GitHub branch)", now
    status = app.get("status", {})
    sync = status.get("sync", {})
    try:
        timestamp = datetime.datetime.fromisoformat(status["reconciledAt"].replace("Z", "+00:00"))
        if timestamp.tzinfo is None:
            raise ValueError("timezone missing")
        reconciled = timestamp.timestamp()
        if reconciled > now + 60:
            raise ValueError("clock ahead")
    except (KeyError, ValueError, TypeError, AttributeError):
        return "reconciliation timestamp unavailable or invalid", now
    if now - reconciled >= threshold:
        return "reconciliation stale since " + iso(reconciled), reconciled
    if sync.get("revision") != head:
        # Never forward arbitrary API error/condition text to the alert sink.
        return "Git HEAD " + head[:12] + " differs from compared revision", now
    if sync.get("status") != "Synced":
        return "sync status is not Synced at Git HEAD " + head[:12], now
    # Synced is comparison, not rollout readiness. An older successful operation
    # is legitimate when unrelated repo changes produce identical app manifests.
    return "", now


def advance(previous, problem, since, now, threshold):
    state = copy.deepcopy(previous)
    event = None
    if problem:
        state["first_bad"] = min(state.get("first_bad", now), since, now)
        if (now - state["first_bad"] >= threshold and
                ("last_alert" not in state or now - state["last_alert"] >= 21600)):
            event = {"kind": "gitops_stalled", "text": problem + "; unhealthy since " + iso(state["first_bad"])}
            state["last_alert"] = now
    else:
        if "last_alert" in state:
            event = {"kind": "gitops_recovered", "text": "Git HEAD compared, Synced, reconciliation fresh"}
        state.pop("first_bad", None)
        state.pop("last_alert", None)
    state["observed_at"] = now
    return state, event


class RequestError(Exception):
    def __init__(self, service, status=None):
        self.status = status
        # Neither credentials, response bodies, nor URLs belong in logs.
        super().__init__(service + (" HTTP " + str(status) if status else " request failed"))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class HTTP:
    def request(self, service, url, token="", method="GET", body=None, ca=None, raw=False):
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password:
            raise RequestError(service)
        headers = {"Accept": "application/json", "User-Agent": "iterion-argocd-liveness"}
        if token:
            headers["Authorization"] = "Bearer " + token
        if body is not None:
            headers["Content-Type"] = "application/merge-patch+json" if method == "PATCH" else "application/json"
        try:
            request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                             headers=headers, method=method)
            opener = urllib.request.build_opener(NoRedirect(), urllib.request.HTTPSHandler(
                context=ssl.create_default_context(cafile=ca)))
            with opener.open(request, timeout=15) as response:
                data = response.read(1048577)
                if len(data) > 1048576:
                    raise ValueError("response too large")
                return None if raw else json.loads(data)
        except urllib.error.HTTPError as exc:
            raise RequestError(service, exc.code) from None
        except (OSError, ValueError, urllib.error.URLError):
            raise RequestError(service) from None


class Probe:
    def __init__(self, config, http=None, clock=time.time):
        self.config = config
        self.http = http or HTTP()
        self.clock = clock
        self.api = "https://kubernetes.default.svc"
        self.sa = pathlib.Path("/var/run/secrets/kubernetes.io/serviceaccount")
        self.token = (self.sa / "token").read_text().strip()
        self.ca = str(self.sa / "ca.crt")
        namespace = (self.sa / "namespace").read_text().strip()
        self.states_url = self.api + "/api/v1/namespaces/" + urllib.parse.quote(namespace, safe="") + "/configmaps"
        self.state_url = self.states_url + "/" + urllib.parse.quote(config["stateName"], safe="")
        self.owner = str(uuid.uuid4())
        self.webhook = pathlib.Path("/credentials/webhook/url").read_text().strip()
        token_file = pathlib.Path("/credentials/github/token")
        self.github_token = token_file.read_text().strip() if token_file.exists() else ""

    def kube(self, url, method="GET", body=None):
        return self.http.request("Kubernetes", url, self.token, method, body, self.ca)

    def save(self, cm, state, owner="", until=0):
        return self.kube(self.state_url, "PATCH", {
            "metadata": {"resourceVersion": cm["metadata"]["resourceVersion"]},
            "data": {"state": json.dumps(state), "owner": owner, "until": str(until)}})

    def notify(self, event):
        label = self.config["applicationNamespace"] + "/" + self.config["application"]
        self.http.request("ops webhook", self.webhook, method="POST", raw=True,
                          body={"text": "[" + event["kind"] + "] " + label + ": " + event["text"]})

    def run(self):
        now = self.clock()
        try:
            cm = self.kube(self.state_url)
        except RequestError as exc:
            if exc.status != 404:
                raise
            # Created by the probe, not Helm/Argo: reconcile/upgrade must never
            # overwrite the observed incident age or notification receipt.
            try:
                cm = self.kube(self.states_url, "POST", {
                    "apiVersion": "v1", "kind": "ConfigMap",
                    "metadata": {"name": self.config["stateName"]}, "data": {}})
            except RequestError as conflict:
                if conflict.status == 409:
                    return "busy"
                raise
        data = cm.get("data", {})
        if float(data.get("until", 0)) > now:
            return "busy"
        state = json.loads(data.get("state", "{}"))
        if not isinstance(state, dict) or any(
                not isinstance(state[key], (int, float)) for key in
                ("first_bad", "last_alert", "observed_at") if key in state):
            raise ValueError("invalid persisted state")
        try:
            # First-writer wins even when Kubernetes creates duplicate Jobs.
            # 180s lease exceeds the chart's 120s activeDeadlineSeconds.
            cm = self.save(cm, state, self.owner, now + 180)
        except RequestError as conflict:
            if conflict.status == 409:
                return "busy"
            raise
        try:
            repo = self.config["repository"]
            branch = self.config["branch"]
            try:
                ref = self.http.request("GitHub", "https://api.github.com/repos/" + repo +
                                        "/git/ref/heads/" + urllib.parse.quote(branch, safe=""), self.github_token)
                head = ref["object"]["sha"]
                if not re.fullmatch(r"[0-9a-f]{40,64}", head):
                    raise ValueError("invalid HEAD")
                namespace = urllib.parse.quote(self.config["applicationNamespace"], safe="")
                app = self.kube(self.api + "/apis/argoproj.io/v1alpha1/namespaces/" + namespace +
                                "/applications/" + urllib.parse.quote(self.config["application"], safe=""))
                problem, since = assess(app, head, repo, branch, now, self.config["thresholdSeconds"])
            except RequestError as exc:
                problem, since = "observation unavailable: " + str(exc), now
            except (KeyError, TypeError, ValueError, AttributeError):
                problem, since = "observation unavailable: invalid API response", now
            new_state, event = advance(state, problem, since, now, self.config["thresholdSeconds"])
            if event:
                self.notify(event)  # a failed POST never burns the receipt
            self.save(cm, new_state)
            return "unhealthy" if problem else "healthy"
        except Exception:
            # Preserve pre-delivery state and release for retry. If Kubernetes
            # is down the finite lease expires without manual intervention.
            try:
                self.save(cm, state)
            except Exception:
                pass
            raise


def main():
    probe = None
    try:
        config = json.loads(pathlib.Path("/config/config.json").read_text())
        probe = Probe(config)
        result = probe.run()
        print(json.dumps({"result": result}))
        return 0
    except Exception:
        # Configuration/state/delivery failures must be visible as failed Jobs,
        # without leaking a request URL, response, or mounted credential.
        print('{"result":"probe_failed","message":"check credentials, state API and webhook"}', file=sys.stderr)
        if probe:
            try:
                probe.notify({"kind": "gitops_probe_failed", "text": "probe failed; check Job logs and credentials"})
            except Exception:
                pass
        return 1


if __name__ == "__main__":
    sys.exit(main())
