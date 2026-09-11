from __future__ import annotations

import base64
import importlib.util
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock


MODULE_PATH = Path(__file__).with_name("instances.py")
SPEC = importlib.util.spec_from_file_location("iterion_instances", MODULE_PATH)
assert SPEC and SPEC.loader
instances = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(instances)


class FakeStudio:
    def __init__(self, project: Path, mission_status: int = 404):
        self.project = project
        self.mission_status = mission_status
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, _format: str, *_args: object) -> None:
                pass

            def send(self, status: int, payload: object) -> None:
                raw = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_GET(self) -> None:
                if self.path == "/api/server/info":
                    self.send(200, {"work_dir": str(outer.project), "commit": "deadbeef", "recovery_passive": False})
                elif self.path.endswith("/assistant-missions"):
                    self.send(outer.mission_status, [] if outer.mission_status == 200 else {"error": "not found"})
                elif self.path == "/api/runs/run-1":
                    self.send(200, {"id": "run-1", "status": "failed"})
                elif self.path == "/api/runs":
                    self.send(200, {"runs": []})
                else:
                    self.send(404, {"error": "not found"})

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def port(self) -> int:
        return self.server.server_address[1]

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


class ManagerFixture:
    def __init__(self, test: unittest.TestCase, duplicate: bool = False):
        self.test = test
        test_tmp = Path(os.environ.get("ITERION_TEST_TMPDIR", "/var/tmp"))
        self.temp = tempfile.TemporaryDirectory(prefix="iterion-manager-test-", dir=test_tmp)
        self.root = Path(self.temp.name)
        self.project = self.root / "project"
        self.project.mkdir()
        self.state = self.root / "state"
        self.worktrees = self.root / "worktrees"
        self.repo = self.root / "engine"
        self.repo.mkdir()
        subprocess.run(["git", "init", "-q", "-b", "base", str(self.repo)], check=True)
        subprocess.run(["git", "-C", str(self.repo), "config", "user.email", "test@example.invalid"], check=True)
        subprocess.run(["git", "-C", str(self.repo), "config", "user.name", "Test"], check=True)
        subprocess.run(["git", "-C", str(self.repo), "config", "commit.gpgsign", "false"], check=True)
        (self.repo / "seed").write_text("seed\n")
        subprocess.run(["git", "-C", str(self.repo), "add", "seed"], check=True)
        subprocess.run(["git", "-C", str(self.repo), "commit", "-q", "-m", "seed"], check=True)
        self.server = FakeStudio(self.project)
        self.config = self.root / "instances.conf"
        lines = [f"ITERION_BIN=/bin/true", f"project {self.project} {self.server.port} no-env"]
        if duplicate:
            lines.append(f"duplicate {self.project} {self.server.port + 1} no-env")
        self.config.write_text("\n".join(lines) + "\n")
        self.profiles = self.root / "profiles.toml"
        self.profiles.write_text(
            "schema_version = 1\n\n"
            "[bindings.project]\nproject_id = \"project\"\ndeployment_profile = \"dev\"\n\n"
            "[profiles.dev]\n"
            f"source_repository = {json.dumps(str(self.repo))}\n"
            "integration_base_ref = \"refs/heads/base\"\n"
            f"worktree_root = {json.dumps(str(self.worktrees))}\n"
            "build_output = \"iterion\"\n"
        )
        (self.project / "iterion.project.toml").write_text(
            "schema_version = 1\nproject_id = \"project\"\ndeployment_profile = \"dev\"\n"
        )
        self.env = mock.patch.dict(
            os.environ,
            {
                "ITERION_INSTANCES_CONFIG": str(self.config),
                "ITERION_DEPLOYMENT_PROFILES_CONFIG": str(self.profiles),
                "ITERION_INSTANCES_STATE": str(self.state),
            },
        )
        self.env.start()

    def close(self) -> None:
        self.env.stop()
        self.server.close()
        self.temp.cleanup()


class InstanceManagerTests(unittest.TestCase):
    def setUp(self) -> None:
        self.fixture = ManagerFixture(self)

    def tearDown(self) -> None:
        self.fixture.close()

    def manager(self) -> instances.Manager:
        manager = instances.Manager()
        manager.pid_path(manager.instances[0]).parent.mkdir(parents=True, exist_ok=True)
        manager.pid_path(manager.instances[0]).write_text(str(os.getpid()))
        return manager

    def test_context_resolves_real_path_and_404_capability(self) -> None:
        alias = self.fixture.root / "project-link"
        alias.symlink_to(self.fixture.project, target_is_directory=True)
        value = self.manager().context(str(alias), "run-1")
        self.assertEqual(value["project"]["root"], str(self.fixture.project.resolve()))
        self.assertEqual(value["instance"]["name"], "project")
        self.assertEqual(value["assistant_missions"], {"state": "absent-recoverable", "http_status": 404})
        self.assertTrue(value["server"]["work_dir_matches"])
        self.assertEqual(len(value["context_token"]), 64)

    def test_context_discovers_project_from_run_across_managed_instances(self) -> None:
        manager = self.manager()
        with mock.patch.object(instances.os, "getcwd", return_value=str(self.fixture.root)):
            value = manager.context(None, "run-1")
        self.assertEqual(value["project"]["root"], str(self.fixture.project.resolve()))
        self.assertEqual(value["run"]["data"]["id"], "run-1")

    def test_capability_requires_documented_list(self) -> None:
        manager = self.manager()
        self.fixture.server.mission_status = 200
        self.assertEqual(manager.capability(manager.instances[0], "run-1")["state"], "present")
        with mock.patch.object(manager, "http_json", return_value=(200, {"unexpected": []})):
            self.assertEqual(manager.capability(manager.instances[0], "run-1")["state"], "indeterminate")
        with mock.patch.object(manager, "http_json", return_value=(403, {})):
            self.assertEqual(manager.capability(manager.instances[0], "run-1")["state"], "unauthorized")

    def test_nested_run_response_is_summarized_without_large_payloads(self) -> None:
        summary = instances.Manager.summarize_run({
            "run": {"id": "run-1", "status": "failed", "inputs": {"large": "payload"}},
            "diagnostic": {"recoverable": False, "evidence": [1, 2, 3]},
            "executions": [{"many": "events"}],
        })
        self.assertEqual(summary, {"id": "run-1", "status": "failed", "diagnostic": {"recoverable": False}})

    def test_runtime_selector_preserves_an_instance_specific_binary(self) -> None:
        manager = self.manager()
        binary = self.fixture.root / "slot" / "iterion"
        binary.parent.mkdir()
        binary.write_bytes(b"instance-specific")
        binary.chmod(0o555)
        manager.record_runtime_binary(manager.instances[0], binary, "test")
        self.assertEqual(manager.selected_binary(manager.instances[0]), binary.resolve())

    def test_foreign_adoption_requires_explicit_pid_and_matching_project(self) -> None:
        manager = instances.Manager()
        receipt = {"name": "project", "pid": 4242, "binary": "/slot/iterion"}
        with mock.patch.object(manager, "state", return_value=("foreign", None)), \
             mock.patch.object(manager, "process_alive", return_value=True), \
             mock.patch.object(manager, "http_json", return_value=(200, {"work_dir": str(self.fixture.project)})), \
             mock.patch.object(manager, "verify_adoption") as verify, \
             mock.patch.object(manager, "_adopt_pid", return_value=receipt):
            result = manager.adopt_process(str(self.fixture.project), 4242)
        verify.assert_called_once()
        self.assertTrue(result["adopted_foreign_process"])
        self.assertEqual(manager.pid_path(manager.instances[0]).read_text(), "4242")

    def test_duplicate_canonical_project_is_rejected(self) -> None:
        self.fixture.close()
        self.fixture = ManagerFixture(self, duplicate=True)
        with self.assertRaises(instances.ManagerError) as caught:
            instances.Manager().resolve(str(self.fixture.project))
        self.assertEqual(caught.exception.code, "PROJECT_INSTANCE_NOT_UNIQUE")

    def test_manifest_rejects_operational_keys(self) -> None:
        with (self.fixture.project / "iterion.project.toml").open("a") as stream:
            stream.write("port = 4894\n")
        with self.assertRaises(instances.ManagerError) as caught:
            instances.Manager().resolve(str(self.fixture.project))
        self.assertEqual(caught.exception.code, "PROJECT_MANIFEST_INVALID")

    def test_prepare_creates_owned_clean_worktree_and_is_idempotent(self) -> None:
        manager = self.manager()
        first = manager.prepare(str(self.fixture.project), "goal-123")
        second = manager.prepare(str(self.fixture.project), "goal-123")
        self.assertFalse(first["reused"])
        self.assertTrue(second["reused"])
        self.assertEqual(first["base_sha"], subprocess.check_output(["git", "-C", str(self.fixture.repo), "rev-parse", "refs/heads/base"], text=True).strip())
        self.assertEqual(subprocess.check_output(["git", "-C", first["worktree"], "status", "--porcelain"], text=True), "")

    def test_build_rejects_dirty_or_abbreviated_commit_before_toolchain(self) -> None:
        manager = self.manager()
        record = manager.prepare(str(self.fixture.project), "goal-build")
        head = subprocess.check_output(["git", "-C", record["worktree"], "rev-parse", "HEAD"], text=True).strip()
        with self.assertRaises(instances.ManagerError) as abbreviated:
            manager.build(str(self.fixture.project), "goal-build", head[:12])
        self.assertEqual(abbreviated.exception.code, "COMMIT_NOT_EXACT")
        (Path(record["worktree"]) / "dirty").write_text("dirty")
        with self.assertRaises(instances.ManagerError) as dirty:
            manager.build(str(self.fixture.project), "goal-build", head)
        self.assertEqual(dirty.exception.code, "WORKTREE_NOT_CLEAN")

    def test_foreign_instance_is_visible_but_not_capability_probed(self) -> None:
        manager = instances.Manager()
        value = manager.context(str(self.fixture.project), "run-1")
        self.assertEqual(value["instance"]["state"], "foreign")
        self.assertEqual(value["assistant_missions"]["state"], "not-probed")

    def test_deploy_success_sets_immutable_selector(self) -> None:
        manager = self.manager()
        instance = manager.instances[0]
        artifact_dir = self.fixture.state / "artifacts" / "artifact-1"
        artifact_dir.mkdir(parents=True)
        artifact = artifact_dir / "iterion"
        artifact.write_bytes(b"candidate")
        artifact.chmod(0o555)
        digest = instances.sha256_file(artifact)
        instances.atomic_json(artifact_dir / "metadata.json", {
            "binary": str(artifact), "sha256": digest, "commit": "a" * 40,
            "preflight": {"recovery_passive": True},
        })
        old = self.fixture.root / "old" / "iterion"
        old.parent.mkdir()
        old.write_bytes(b"old")
        old.chmod(0o555)
        env_file = self.fixture.root / "old" / "env.json"
        instances.atomic_json(env_file, {
            "schema_version": 1,
            "format": "nul-base64-v1",
            "data": base64.b64encode(f"PATH={os.environ.get('PATH', '')}\0".encode()).decode(),
        })
        context = {
            "context_token": "token", "instance": {"state": "up", "pid": 123},
            "server": {"reachable": True, "work_dir_matches": True},
            "assistant_missions": {"state": "absent-recoverable"},
            "run": {"data": {"id": "run-1", "status": "failed"}},
        }
        with mock.patch.object(manager, "context", return_value=context), \
             mock.patch.object(manager, "active_runs", return_value=[]), \
             mock.patch.object(manager, "verify_adoption"), \
             mock.patch.object(manager, "freeze_runtime", return_value={"binary": str(old), "sha256": instances.sha256_file(old), "environment_file": str(env_file)}), \
             mock.patch.object(manager, "stop_pid"), \
             mock.patch.object(manager, "spawn", return_value=456), \
             mock.patch.object(manager, "process_binary_hash", return_value=digest), \
             mock.patch.object(manager, "wait_ready", return_value={"work_dir": str(self.fixture.project), "recovery_passive": False, "commit": "a" * 12}), \
             mock.patch.object(manager, "capability", return_value={"state": "present", "http_status": 200}):
            result = manager.deploy(str(self.fixture.project), "artifact-1", "token", "goal-1", "run-1")
        self.assertTrue(result["deployed"])
        selector = json.loads((self.fixture.state / "deployments/project/current.json").read_text())
        self.assertEqual(selector["sha256"], digest)

    def test_deploy_failure_rolls_back_and_reports_stable_error(self) -> None:
        manager = self.manager()
        artifact_dir = self.fixture.state / "artifacts" / "artifact-2"
        artifact_dir.mkdir(parents=True)
        artifact = artifact_dir / "iterion"
        artifact.write_bytes(b"candidate")
        artifact.chmod(0o555)
        digest = instances.sha256_file(artifact)
        instances.atomic_json(artifact_dir / "metadata.json", {
            "binary": str(artifact), "sha256": digest, "commit": "b" * 40,
            "preflight": {"recovery_passive": True},
        })
        old = self.fixture.root / "old" / "iterion"
        old.parent.mkdir()
        old.write_bytes(b"old")
        old.chmod(0o555)
        env_file = self.fixture.root / "old" / "env.json"
        instances.atomic_json(env_file, {
            "schema_version": 1,
            "format": "nul-base64-v1",
            "data": base64.b64encode(f"PATH={os.environ.get('PATH', '')}\0".encode()).decode(),
        })
        context = {
            "context_token": "token", "instance": {"state": "up", "pid": 123},
            "server": {"reachable": True, "work_dir_matches": True},
            "assistant_missions": {"state": "absent-recoverable"},
            "run": {"data": {"id": "run-1", "status": "failed"}},
        }
        ready_calls = [instances.ManagerError("LIVE_VERIFICATION_FAILED", "candidate failed"), {"work_dir": str(self.fixture.project)}]
        def ready(*_args: object, **_kwargs: object) -> dict[str, object]:
            value = ready_calls.pop(0)
            if isinstance(value, Exception):
                raise value
            return value
        with mock.patch.object(manager, "context", return_value=context), \
             mock.patch.object(manager, "active_runs", return_value=[]), \
             mock.patch.object(manager, "verify_adoption"), \
             mock.patch.object(manager, "freeze_runtime", return_value={"binary": str(old), "sha256": instances.sha256_file(old), "environment_file": str(env_file)}), \
             mock.patch.object(manager, "stop_pid"), \
             mock.patch.object(manager, "pid", return_value=456), \
             mock.patch.object(manager, "spawn", side_effect=[456, 789]), \
             mock.patch.object(manager, "process_binary_hash", return_value=instances.sha256_file(old)), \
             mock.patch.object(manager, "wait_ready", side_effect=ready):
            with self.assertRaises(instances.ManagerError) as caught:
                manager.deploy(str(self.fixture.project), "artifact-2", "token", "goal-1", "run-1")
        self.assertEqual(caught.exception.code, "DEPLOYMENT_ROLLED_BACK")
        journals = list((self.fixture.state / "deployments/project/transactions").glob("*/journal.json"))
        self.assertEqual(len(journals), 1)
        self.assertEqual(json.loads(journals[0].read_text())["phase"], "rolled-back")

    def test_failed_rollback_leaves_recovery_required_journal(self) -> None:
        manager = self.manager()
        artifact_dir = self.fixture.state / "artifacts" / "artifact-3"
        artifact_dir.mkdir(parents=True)
        artifact = artifact_dir / "iterion"
        artifact.write_bytes(b"candidate")
        artifact.chmod(0o555)
        digest = instances.sha256_file(artifact)
        instances.atomic_json(artifact_dir / "metadata.json", {
            "binary": str(artifact), "sha256": digest, "commit": "c" * 40,
            "preflight": {"recovery_passive": True},
        })
        old = self.fixture.root / "old" / "iterion"
        old.parent.mkdir()
        old.write_bytes(b"old")
        old.chmod(0o555)
        env_file = self.fixture.root / "old" / "env.json"
        instances.atomic_json(env_file, {
            "schema_version": 1,
            "format": "nul-base64-v1",
            "data": base64.b64encode(f"PATH={os.environ.get('PATH', '')}\0".encode()).decode(),
        })
        context = {
            "context_token": "token", "instance": {"state": "up", "pid": 123},
            "server": {"reachable": True, "work_dir_matches": True},
            "assistant_missions": {"state": "absent-recoverable"},
            "run": {"data": {"id": "run-1", "status": "failed"}},
        }
        ready_calls = [instances.ManagerError("LIVE_VERIFICATION_FAILED", "candidate failed"), {"work_dir": str(self.fixture.project)}]
        def ready(*_args: object, **_kwargs: object) -> dict[str, object]:
            value = ready_calls.pop(0)
            if isinstance(value, Exception):
                raise value
            return value
        with mock.patch.object(manager, "context", return_value=context), \
             mock.patch.object(manager, "active_runs", return_value=[]), \
             mock.patch.object(manager, "verify_adoption"), \
             mock.patch.object(manager, "freeze_runtime", return_value={"binary": str(old), "sha256": instances.sha256_file(old), "environment_file": str(env_file)}), \
             mock.patch.object(manager, "stop_pid"), \
             mock.patch.object(manager, "pid", return_value=456), \
             mock.patch.object(manager, "spawn", side_effect=[456, 789]), \
             mock.patch.object(manager, "process_binary_hash", return_value="wrong"), \
             mock.patch.object(manager, "wait_ready", side_effect=ready):
            with self.assertRaises(instances.ManagerError) as caught:
                manager.deploy(str(self.fixture.project), "artifact-3", "token", "goal-1", "run-1")
        self.assertEqual(caught.exception.code, "ROLLBACK_FAILED")
        journals = list((self.fixture.state / "deployments/project/transactions").glob("*/journal.json"))
        self.assertEqual(json.loads(journals[0].read_text())["phase"], "recovery-required")


if __name__ == "__main__":
    unittest.main()
