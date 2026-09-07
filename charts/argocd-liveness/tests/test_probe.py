import copy
import importlib.util
import pathlib
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "probe", pathlib.Path(__file__).parents[1] / "files" / "probe.py")
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)

NOW = 1788800000
HEAD = "a" * 40


def application():
    return {"spec": {"source": {"repoURL": "https://github.com/acme/infra.git",
                               "targetRevision": "master"}},
            "status": {"sync": {"revision": HEAD, "status": "Synced"},
                       "reconciledAt": probe.iso(NOW)}}


class ProbeTest(unittest.TestCase):
    def assess(self, app):
        return probe.assess(app, HEAD, "acme/infra", "master", NOW, 900)

    def test_live_equal_head_is_healthy_without_requiring_a_new_sync_operation(self):
        # A repo-wide revision can change without changing this app's manifests.
        app = application()
        app["status"]["operationState"] = {"phase": "Succeeded", "syncResult": {"revision": "b" * 40}}
        self.assertEqual(self.assess(app), ("", NOW))

    def test_equal_revision_is_insufficient(self):
        for status in ["OutOfSync", "Unknown", ""]:
            with self.subTest(status=status):
                app = application()
                app["status"]["sync"]["status"] = status
                self.assertIn("sync status", self.assess(app)[0])

    def test_stale_reconciliation_and_missing_or_future_timestamp_fail_closed(self):
        for timestamp in [probe.iso(NOW - 901), None, "invalid", probe.iso(NOW + 600)]:
            app = application()
            app["status"]["reconciledAt"] = timestamp
            self.assertTrue(self.assess(app)[0])

    def test_wrong_source_and_multiple_sources_are_not_monitored_as_healthy(self):
        for change in [{"sources": [{"repoURL": "other"}]},
                       {"source": {"repoURL": "https://github.com/other/infra.git", "targetRevision": "master"}},
                       {"source": {"repoURL": "https://github.com/acme/infra.git", "targetRevision": "main"}}]:
            app = application()
            app["spec"].update(change)
            self.assertIn("source", self.assess(app)[0])

    def test_continuous_pushes_do_not_reset_incident_age(self):
        state, event = probe.advance({}, "behind head A", NOW, NOW, 900)
        self.assertIsNone(event)
        state, event = probe.advance(state, "behind head B", NOW + 899, NOW + 899, 900)
        self.assertIsNone(event)
        state, event = probe.advance(state, "behind head C", NOW + 900, NOW + 900, 900)
        self.assertEqual(event["kind"], "gitops_stalled")
        self.assertEqual(state["first_bad"], NOW)

    def test_stale_app_alerts_on_first_observation_then_dedups_and_recovers(self):
        state, event = probe.advance({}, "stale reconciliation", NOW - 1000, NOW, 900)
        self.assertEqual(event["kind"], "gitops_stalled")
        state, event = probe.advance(state, "still stale", NOW, NOW + 300, 900)
        self.assertIsNone(event)
        state, event = probe.advance(state, "", NOW + 600, NOW + 600, 900)
        self.assertEqual(event["kind"], "gitops_recovered")
        self.assertNotIn("first_bad", state)
        state, event = probe.advance(state, "new incident", NOW + 700, NOW + 700, 900)
        self.assertIsNone(event)

    def test_state_is_not_modified_until_delivery_succeeds(self):
        state = {"first_bad": NOW - 1000}
        before = copy.deepcopy(state)
        new, event = probe.advance(state, "unavailable", NOW, NOW, 900)
        self.assertEqual(state, before)
        self.assertIn("last_alert", new)
        self.assertIsNotNone(event)

    def test_reminder_is_bounded_and_no_recovery_for_short_transient(self):
        state, _ = probe.advance({}, "unavailable", NOW, NOW, 900)
        state, event = probe.advance(state, "", NOW + 60, NOW + 60, 900)
        self.assertIsNone(event)
        state, _ = probe.advance({}, "unavailable", NOW - 1000, NOW, 900)
        _, event = probe.advance(state, "unavailable", NOW + 21600, NOW + 21600, 900)
        self.assertEqual(event["kind"], "gitops_stalled")


class FakeHTTP:
    def __init__(self):
        self.cm = {"metadata": {"resourceVersion": "1"}, "data": {}}
        self.now = NOW
        self.head = HEAD
        self.app = application()
        self.posts = []
        self.fail_post = False
        self.fail_read = False
        self.conflict = False

    def request(self, service, url, token="", method="GET", body=None, ca=None, raw=False):
        if service == "ops webhook":
            if self.fail_post:
                raise probe.RequestError(service, 503)
            self.posts.append(body)
            return None
        if service == "GitHub":
            if self.fail_read:
                raise probe.RequestError(service, 401)
            return {"object": {"sha": self.head}}
        if "/applications/" in url:
            self.app["status"]["reconciledAt"] = probe.iso(self.now)
            return copy.deepcopy(self.app)
        if method == "GET":
            if self.cm is None:
                raise probe.RequestError(service, 404)
            return copy.deepcopy(self.cm)
        if method == "POST":
            self.cm = copy.deepcopy(body)
            self.cm["metadata"]["resourceVersion"] = "1"
            return copy.deepcopy(self.cm)
        if method == "PATCH":
            if self.conflict or body["metadata"]["resourceVersion"] != self.cm["metadata"]["resourceVersion"]:
                raise probe.RequestError(service, 409)
            self.cm["metadata"]["resourceVersion"] = str(int(self.cm["metadata"]["resourceVersion"]) + 1)
            self.cm["data"] = copy.deepcopy(body["data"])
            return copy.deepcopy(self.cm)
        raise AssertionError((service, url, method))


class WiringTest(unittest.TestCase):
    def setUp(self):
        self.http = FakeHTTP()
        self.config = {"application": "app", "applicationNamespace": "argocd",
                       "repository": "acme/infra", "branch": "master",
                       "thresholdSeconds": 900, "stateName": "probe-state"}
        def read(path):
            return {"token": "test-token", "namespace": "ops", "url": "https://ops.example/hook"}[path.name]
        with mock.patch.object(pathlib.Path, "read_text", read), mock.patch.object(pathlib.Path, "exists", return_value=False):
            self.probe = probe.Probe(self.config, self.http, lambda: self.http.now)

    def state(self):
        return probe.json.loads(self.http.cm["data"]["state"])

    def test_full_pipeline_create_stall_alert_dedup_recovery(self):
        self.http.cm = None
        self.http.head = "b" * 40
        self.assertEqual(self.probe.run(), "unhealthy")
        self.http.now += 901
        self.http.head = "c" * 40
        self.probe.run()
        self.assertIn("gitops_stalled", self.http.posts[0]["text"])
        self.probe.run()
        self.assertEqual(len(self.http.posts), 1)
        self.http.head = HEAD
        self.assertEqual(self.probe.run(), "healthy")
        self.assertIn("gitops_recovered", self.http.posts[1]["text"])
        self.assertNotIn("first_bad", self.state())

    def test_api_failure_is_not_a_healthy_observation(self):
        self.http.fail_read = True
        self.assertEqual(self.probe.run(), "unhealthy")
        self.http.now += 900
        self.probe.run()
        self.assertIn("GitHub HTTP 401", self.http.posts[0]["text"])

    def test_failed_delivery_releases_claim_preserves_age_and_retries(self):
        self.http.head = "b" * 40
        self.probe.run()
        self.http.now += 901
        self.http.fail_post = True
        with self.assertRaises(probe.RequestError):
            self.probe.run()
        self.assertNotIn("last_alert", self.state())
        self.assertEqual(self.state()["first_bad"], NOW)
        self.assertEqual(self.http.cm["data"]["until"], "0")
        self.http.fail_post = False
        self.probe.run()
        self.assertEqual(len(self.http.posts), 1)

    def test_duplicate_job_and_live_lease_do_not_notify(self):
        self.http.conflict = True
        self.assertEqual(self.probe.run(), "busy")
        self.http.conflict = False
        self.http.cm["data"] = {"until": str(NOW + 180), "owner": "other-job"}
        self.assertEqual(self.probe.run(), "busy")
        self.assertEqual(self.http.posts, [])
        self.http.now += 181
        self.assertEqual(self.probe.run(), "healthy")

    def test_corrupt_state_is_not_silently_reset(self):
        self.http.cm["data"] = {"state": '{"first_bad":"yesterday"}'}
        before = copy.deepcopy(self.http.cm)
        with self.assertRaises(ValueError):
            self.probe.run()
        self.assertEqual(self.http.cm, before)

    def test_http_refuses_insecure_credentials_and_redirects(self):
        with self.assertRaises(probe.RequestError) as error:
            probe.HTTP().request("ops webhook", "http://example.org/secret-token")
        self.assertNotIn("secret-token", str(error.exception))
        self.assertIsNone(probe.NoRedirect().redirect_request(None, None, 302, "", {}, "https://other"))


if __name__ == "__main__":
    unittest.main()
