from __future__ import annotations

import contextlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import iterion_workspacectl as ctl


PROJECTS = [
    {
        "id": "parent-id",
        "name": "workspace",
        "dir": "/work",
        "last_opened": "2026-09-01T00:00:00Z",
    },
    {
        "id": "alpha-id",
        "name": "alpha",
        "dir": "/work/alpha",
        "store_dir": "/stores/alpha",
        "bots_paths": ["/bots/common"],
        "last_opened": "2026-09-02T00:00:00Z",
    },
    {
        "id": "beta-id",
        "name": "beta",
        "dir": "/work/beta",
        "store_dir": "/stores/beta",
        "last_opened": "2026-09-03T00:00:00Z",
    },
]


def completed(argv, code=0, stdout="", stderr=""):
    return subprocess.CompletedProcess(argv, code, stdout, stderr)


class RegistryTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.registry_file = Path(self.tmp.name) / "config.json"
        self.registry = {
            "version": 1,
            "current_project_id": "beta-id",
            "recent_projects": PROJECTS,
            "unrelated_setting": {"preserve": True},
        }
        self.registry_file.write_text(json.dumps(self.registry), encoding="utf-8")

    def test_load_registry_rejects_duplicate_ids(self):
        invalid = dict(self.registry)
        invalid["recent_projects"] = [PROJECTS[0], dict(PROJECTS[0])]
        self.registry_file.write_text(json.dumps(invalid), encoding="utf-8")
        with self.assertRaisesRegex(ctl.WorkspaceError, "missing or duplicated"):
            ctl.load_registry(self.registry_file)

    def test_resolve_project_prefers_deepest_containing_directory(self):
        project = ctl.resolve_project(self.registry, cwd=Path("/work/alpha/src"))
        self.assertEqual(project["id"], "alpha-id")

    def test_resolve_project_by_id_name_and_current(self):
        self.assertEqual(ctl.resolve_project(self.registry, "alpha-id")["name"], "alpha")
        self.assertEqual(ctl.resolve_project(self.registry, "beta")["id"], "beta-id")
        self.assertEqual(ctl.resolve_project(self.registry, use_current=True)["id"], "beta-id")

    def test_resolve_project_does_not_fall_back_to_current(self):
        with self.assertRaisesRegex(ctl.WorkspaceError, "No registered"):
            ctl.resolve_project(self.registry, "/elsewhere")

    def test_resolve_project_rejects_equal_depth_ambiguity(self):
        registry = dict(self.registry)
        registry["recent_projects"] = [
            {"id": "one", "name": "one", "dir": "/work/alpha"},
            {"id": "two", "name": "two", "dir": "/work/alpha"},
        ]
        with self.assertRaisesRegex(ctl.WorkspaceError, "Several registered"):
            ctl.resolve_project(registry, "/work/alpha/src")

    def test_project_url_is_scoped_and_escaped(self):
        self.assertEqual(
            ctl.project_url("alpha id", "http://127.0.0.1:4891"),
            "http://127.0.0.1:4891/x/alpha%20id/",
        )

    def test_context_preserves_registry_bytes_and_reports_degraded(self):
        before = self.registry_file.read_bytes()

        def runner(argv, timeout=None):
            return completed(argv, stdout="active\n")

        def unavailable(url, timeout=None):
            raise ctl.WorkspaceError("WORKSPACE_UNREACHABLE", "down")

        controller = ctl.Controller(
            registry_file=self.registry_file,
            origin="http://127.0.0.1:4891",
            runner=runner,
            fetcher=unavailable,
        )
        value = controller.context("/work/alpha/src")
        self.assertEqual(value["project"]["url"], "http://127.0.0.1:4891/x/alpha-id/")
        self.assertEqual(value["project"]["state"], "unknown")
        self.assertEqual(value["current_project_id"], "beta-id")
        self.assertEqual(self.registry_file.read_bytes(), before)


class ControllerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.registry_file = Path(self.tmp.name) / "config.json"
        self.registry_file.write_text(
            json.dumps(
                {
                    "version": 1,
                    "current_project_id": "alpha-id",
                    "recent_projects": PROJECTS[1:],
                }
            ),
            encoding="utf-8",
        )
        self.calls: list[list[str]] = []

    def runner(self, argv, timeout=None):
        self.calls.append(list(argv))
        if argv[:3] == ["systemctl", "--user", "is-active"]:
            return completed(argv, stdout="active\n")
        return completed(argv)

    @staticmethod
    def ready_fetcher(url, timeout=None):
        if url.endswith("/api/workspace/runtimes"):
            return [
                {"id": "alpha-id", "state": "ready", "runtime_ready": True},
                {"id": "beta-id", "state": "ready", "runtime_ready": True},
            ]
        if url.endswith("/readyz"):
            return {"ready": 2, "degraded_project_ids": []}
        if "/api/runs?" in url:
            return {"runs": []}
        raise AssertionError(url)

    def controller(self, fetcher=None):
        return ctl.Controller(
            registry_file=self.registry_file,
            origin="http://127.0.0.1:4891",
            runner=self.runner,
            fetcher=fetcher or self.ready_fetcher,
            sleeper=lambda _: None,
        )

    def test_status_separates_service_and_project_health(self):
        value = self.controller().status()
        self.assertTrue(value["service"]["active"])
        self.assertTrue(value["service"]["ready"])
        self.assertEqual([item["state"] for item in value["projects"]], ["ready", "ready"])

    def test_restart_mutates_only_the_shared_service(self):
        value = self.controller().mutate("restart")
        self.assertEqual(value["action"], "restart")
        self.assertEqual(value["scope"], "all_registered_projects")
        self.assertIn(["systemctl", "--user", "restart", ctl.SERVICE_NAME], self.calls)

    def test_restart_guard_calls_all_states_twice(self):
        urls: list[str] = []

        def fetcher(url, timeout=None):
            urls.append(url)
            return self.ready_fetcher(url, timeout=timeout)

        self.controller(fetcher).mutate("restart")
        run_urls = [url for url in urls if "/api/runs?" in url]
        self.assertEqual(len(run_urls), 8)
        self.assertEqual(sum("status=running" in url for url in run_urls), 4)
        self.assertEqual(sum("status=queued" in url for url in run_urls), 4)

    def test_active_run_refuses_restart_before_systemd_mutation(self):
        def fetcher(url, timeout=None):
            if "alpha-id" in url and "status=running" in url:
                return {"runs": [{"id": "run-1", "status": "running"}]}
            return self.ready_fetcher(url, timeout=timeout)

        with self.assertRaisesRegex(ctl.WorkspaceError, "runs are running or queued") as caught:
            self.controller(fetcher).mutate("restart")
        self.assertEqual(caught.exception.code, "ACTIVE_RUNS")
        self.assertNotIn(["systemctl", "--user", "restart", ctl.SERVICE_NAME], self.calls)

    def test_partial_run_check_refuses_stop(self):
        def fetcher(url, timeout=None):
            if "beta-id" in url and "status=queued" in url:
                raise ctl.WorkspaceError("WORKSPACE_UNREACHABLE", "down")
            return self.ready_fetcher(url, timeout=timeout)

        with self.assertRaises(ctl.WorkspaceError) as caught:
            self.controller(fetcher).mutate("stop")
        self.assertEqual(caught.exception.code, "WORKSPACE_CHECK_INCOMPLETE")
        self.assertNotIn(["systemctl", "--user", "stop", ctl.SERVICE_NAME], self.calls)

    def test_inconsistent_run_status_makes_check_incomplete(self):
        def fetcher(url, timeout=None):
            if "status=queued" in url:
                return {"runs": [{"id": "run-1", "status": "paused_operator"}]}
            return self.ready_fetcher(url, timeout=timeout)

        with self.assertRaises(ctl.WorkspaceError) as caught:
            self.controller(fetcher).mutate("stop")
        self.assertEqual(caught.exception.code, "WORKSPACE_CHECK_INCOMPLETE")
        self.assertNotIn(["systemctl", "--user", "stop", ctl.SERVICE_NAME], self.calls)

    def test_force_overrides_incomplete_check(self):
        def fetcher(url, timeout=None):
            if "/api/runs?" in url:
                raise ctl.WorkspaceError("WORKSPACE_UNREACHABLE", "down")
            return self.ready_fetcher(url, timeout=timeout)

        value = self.controller(fetcher).mutate("restart", force=True)
        self.assertTrue(value["guard"]["forced"])
        self.assertTrue(value["guard"]["errors"])
        self.assertIn(["systemctl", "--user", "restart", ctl.SERVICE_NAME], self.calls)

    def test_start_does_not_require_reachable_service_beforehand(self):
        states = iter(["inactive", "active"])

        def runner(argv, timeout=None):
            self.calls.append(list(argv))
            if argv[:3] == ["systemctl", "--user", "is-active"]:
                return completed(argv, stdout=next(states) + "\n")
            return completed(argv)

        controller = ctl.Controller(
            registry_file=self.registry_file,
            origin="http://127.0.0.1:4891",
            runner=runner,
            fetcher=self.ready_fetcher,
            sleeper=lambda _: None,
        )
        value = controller.mutate("start")
        self.assertEqual(value["before"], "inactive")
        self.assertEqual(value["after"], "active")

    def test_lock_file_is_private(self):
        path = Path(self.tmp.name) / "controller.lock"
        with ctl.mutation_lock(path):
            self.assertTrue(path.exists())
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)


class CommandOutputTest(unittest.TestCase):
    def test_compat_context_keeps_required_instance_fields(self):
        fake = mock.Mock()
        fake.context.return_value = {
            "service": {"name": ctl.SERVICE_NAME, "origin": ctl.DEFAULT_ORIGIN, "state": "active"},
            "project": {"id": "alpha-id", "dir": "/work/alpha", "url": f"{ctl.DEFAULT_ORIGIN}/x/alpha-id/"},
            "current_project_id": "beta-id",
        }
        stdout = io.StringIO()
        with mock.patch.dict(os.environ, {"ITERION_WORKSPACE_COMPAT": "1"}), mock.patch.object(
            ctl, "Controller", return_value=fake
        ), contextlib.redirect_stdout(stdout):
            code = ctl.main(["context", "--project", "/work/alpha", "--json"])
        payload = json.loads(stdout.getvalue())
        self.assertEqual(code, 0)
        self.assertEqual(payload["schema_version"], 1)
        self.assertEqual(payload["result"]["instance"]["name"], "iterion-workspace")
        self.assertEqual(payload["result"]["instance"]["url"], f"{ctl.DEFAULT_ORIGIN}/x/alpha-id/")

    def test_compat_project_restart_still_targets_shared_service(self):
        fake = mock.Mock()
        fake.registry.return_value = {
            "version": 1,
            "current_project_id": "alpha-id",
            "recent_projects": [
                {"id": "alpha-id", "name": "alpha", "dir": "/work/alpha"}
            ],
        }
        fake.mutate.return_value = {
            "scope": "all_registered_projects",
            "service": ctl.SERVICE_NAME,
        }
        stdout = io.StringIO()
        with mock.patch.dict(os.environ, {"ITERION_WORKSPACE_COMPAT": "1"}), mock.patch.object(
            ctl, "Controller", return_value=fake
        ), contextlib.redirect_stdout(stdout):
            code = ctl.main(["restart", "alpha", "--json"])
        self.assertEqual(code, 0)
        fake.mutate.assert_called_once_with("restart", force=False)
        self.assertEqual(json.loads(stdout.getvalue())["result"]["scope"], "all_registered_projects")

    def test_retired_command_has_structured_json_error(self):
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            code = ctl.main(["deploy", "--json"])
        payload = json.loads(stdout.getvalue())
        self.assertEqual(code, 2)
        self.assertEqual(payload["error"]["code"], "LEGACY_COMMAND_RETIRED")

    def test_unknown_command_has_structured_json_error(self):
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            code = ctl.main(["unknown-command", "--json"])
        payload = json.loads(stdout.getvalue())
        self.assertEqual(code, 2)
        self.assertEqual(payload["error"]["code"], "ARGUMENT_INVALID")

    def test_json_error_does_not_write_prose_to_stdout(self):
        fake = mock.Mock()
        fake.context.side_effect = ctl.WorkspaceError("PROJECT_NOT_REGISTERED", "missing")
        stdout = io.StringIO()
        stderr = io.StringIO()
        with mock.patch.object(ctl, "Controller", return_value=fake), contextlib.redirect_stdout(
            stdout
        ), contextlib.redirect_stderr(stderr):
            code = ctl.main(["context", "--project", "/missing", "--json"])
        self.assertEqual(code, 2)
        self.assertEqual(stderr.getvalue(), "")
        self.assertFalse(json.loads(stdout.getvalue())["ok"])


if __name__ == "__main__":
    unittest.main()
