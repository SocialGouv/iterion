#!/usr/bin/env python3
"""Project-scoped owner for local Iterion Studio instances and bootstrap deploys."""

from __future__ import annotations

import argparse
import base64
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import tomllib
from typing import Any, Iterator
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen
import webbrowser


SCHEMA_VERSION = 1
TERMINAL_RUN_STATES = {"success", "succeeded", "finished", "failed", "cancelled", "canceled"}
HUMAN_GATE_MARKERS = ("waiting_human", "human_input", "operator_pause")
SAFE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
AUTHORITY_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$")
FULL_SHA = re.compile(r"^[0-9a-f]{40}$")


class ManagerError(Exception):
    def __init__(self, code: str, message: str, **details: Any):
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details


def fail(code: str, message: str, **details: Any) -> None:
    raise ManagerError(code, message, **details)


def canonical(path: str | Path) -> Path:
    try:
        return Path(path).expanduser().resolve(strict=True)
    except (OSError, RuntimeError) as exc:
        fail("PATH_NOT_FOUND", f"chemin introuvable: {path}", path=str(path), cause=str(exc))


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def json_hash(value: Any) -> str:
    raw = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(raw).hexdigest()


def atomic_json(path: Path, value: Any, mode: int = 0o600) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, indent=2, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def read_json(path: Path, code: str) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        fail(code, f"état illisible: {path}", cause=str(exc))
    if not isinstance(value, dict):
        fail(code, f"état invalide: {path}")
    return value


def run_checked(args: list[str], cwd: Path | None = None, env: dict[str, str] | None = None) -> str:
    try:
        result = subprocess.run(
            args,
            cwd=cwd,
            env=env,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        stderr = getattr(exc, "stderr", "") or ""
        fail("COMMAND_FAILED", f"commande en échec: {shlex.join(args)}", stderr=stderr[-4000:])
    return result.stdout.strip()


def load_toml(path: Path, expected: set[str], code: str) -> dict[str, Any]:
    try:
        value = tomllib.loads(path.read_text())
    except FileNotFoundError:
        fail(code, f"fichier requis absent: {path}", path=str(path))
    except (OSError, tomllib.TOMLDecodeError) as exc:
        fail(code, f"TOML invalide: {path}", cause=str(exc))
    unknown = set(value) - expected
    if unknown:
        fail(code, f"clés inconnues dans {path}: {', '.join(sorted(unknown))}")
    return value


class Instance:
    def __init__(self, name: str, project_dir: Path, port: int, local_store: bool, no_env: bool, extra: list[str]):
        self.name = name
        self.project_dir = project_dir
        self.port = port
        self.local_store = local_store
        self.no_env = no_env
        self.extra = extra


class Manager:
    def __init__(self) -> None:
        home = Path.home()
        self.config_path = Path(os.environ.get(
            "ITERION_INSTANCES_CONFIG",
            Path(os.environ.get("XDG_CONFIG_HOME", home / ".config")) / "iterion/instances.conf",
        )).expanduser()
        self.profiles_path = Path(os.environ.get(
            "ITERION_DEPLOYMENT_PROFILES_CONFIG",
            Path(os.environ.get("XDG_CONFIG_HOME", home / ".config")) / "iterion/deployment-profiles.toml",
        )).expanduser()
        self.state_dir = Path(os.environ.get("ITERION_INSTANCES_STATE", home / ".iterion/instances")).expanduser()
        self.bind = os.environ.get("BIND", "127.0.0.1")
        self.load_project_env = os.environ.get("LOAD_PROJECT_ENV", "1") == "1"
        self.configured_bin = os.environ.get("ITERION_BIN", "")
        self.instances: list[Instance] = []
        self._load_instances()

    def _load_instances(self) -> None:
        if not self.config_path.is_file():
            fail("INSTANCE_CONFIG_MISSING", f"config introuvable: {self.config_path}")
        names: set[str] = set()
        for number, raw in enumerate(self.config_path.read_text().splitlines(), 1):
            stripped = raw.strip()
            if not stripped or stripped.startswith("#"):
                continue
            directive = re.fullmatch(r"([A-Za-z_][A-Za-z0-9_]*)=(.*)", stripped)
            if directive:
                key, value = directive.groups()
                if key == "ITERION_BIN":
                    self.configured_bin = str(Path(value).expanduser())
                elif key == "LOAD_PROJECT_ENV":
                    self.load_project_env = value == "1"
                elif key == "BIND":
                    self.bind = value
                else:
                    fail("INSTANCE_CONFIG_INVALID", f"directive inconnue ligne {number}: {key}")
                continue
            try:
                fields = shlex.split(raw, comments=True)
            except ValueError as exc:
                fail("INSTANCE_CONFIG_INVALID", f"ligne {number} invalide", cause=str(exc))
            if len(fields) < 3:
                fail("INSTANCE_CONFIG_INVALID", f"ligne {number} incomplète")
            name, directory, port_text, *rest = fields
            if name in names or not SAFE_ID.fullmatch(name):
                fail("INSTANCE_CONFIG_INVALID", f"nom invalide ou dupliqué ligne {number}: {name}")
            try:
                port = int(port_text)
            except ValueError:
                fail("INSTANCE_CONFIG_INVALID", f"port invalide ligne {number}: {port_text}")
            if not 1 <= port <= 65535:
                fail("INSTANCE_CONFIG_INVALID", f"port hors limites ligne {number}: {port}")
            local_store = "local-store" in rest
            no_env = "no-env" in rest
            extra = [str(Path(token[1:]).expanduser()) if token.startswith("~/") else token for token in rest if token not in {"local-store", "no-env"}]
            self.instances.append(Instance(name, Path(directory).expanduser().resolve(), port, local_store, no_env, extra))
            names.add(name)
        if not self.instances:
            fail("INSTANCE_CONFIG_INVALID", "aucune instance déclarée")

    def load_profiles(self) -> tuple[dict[str, Any], dict[str, Any]]:
        data = load_toml(self.profiles_path, {"schema_version", "bindings", "profiles"}, "DEPLOYMENT_CONFIG_INVALID")
        if data.get("schema_version") != SCHEMA_VERSION:
            fail("DEPLOYMENT_CONFIG_INVALID", "schema_version de déploiement non supportée")
        bindings = data.get("bindings")
        profiles = data.get("profiles")
        if not isinstance(bindings, dict) or not isinstance(profiles, dict):
            fail("DEPLOYMENT_CONFIG_INVALID", "bindings et profiles doivent être des tables")
        for name, binding in bindings.items():
            self._strict_table(binding, {"project_id", "deployment_profile"}, f"bindings.{name}")
        for name, profile in profiles.items():
            self._strict_table(profile, {"source_repository", "integration_base_ref", "worktree_root", "build_output"}, f"profiles.{name}")
            if profile["build_output"] != "iterion":
                fail("DEPLOYMENT_CONFIG_INVALID", f"profiles.{name}.build_output doit être iterion")
        return bindings, profiles

    @staticmethod
    def _strict_table(value: Any, keys: set[str], label: str) -> None:
        if not isinstance(value, dict) or set(value) != keys or not all(isinstance(value[key], str) and value[key] for key in keys):
            fail("DEPLOYMENT_CONFIG_INVALID", f"table {label} incomplète ou avec clés inconnues")

    def manifest(self, root: Path) -> dict[str, str]:
        path = root / "iterion.project.toml"
        data = load_toml(path, {"schema_version", "project_id", "deployment_profile"}, "PROJECT_MANIFEST_INVALID")
        if data.get("schema_version") != SCHEMA_VERSION:
            fail("PROJECT_MANIFEST_INVALID", "schema_version projet non supportée")
        for key in ("project_id", "deployment_profile"):
            if not isinstance(data.get(key), str) or not data[key] or not SAFE_ID.fullmatch(data[key]):
                fail("PROJECT_MANIFEST_INVALID", f"{key} invalide dans {path}")
        return {"project_id": data["project_id"], "deployment_profile": data["deployment_profile"]}

    def resolve(self, project: str | None) -> tuple[Path, Instance, dict[str, str], dict[str, Any]]:
        root = canonical(project or os.getcwd())
        matches = [instance for instance in self.instances if canonical(instance.project_dir) == root]
        if len(matches) != 1:
            fail("PROJECT_INSTANCE_NOT_UNIQUE", "le chemin réel du projet doit correspondre à une seule instance", project=str(root), matches=[item.name for item in matches])
        instance = matches[0]
        manifest = self.manifest(root)
        bindings, profiles = self.load_profiles()
        binding = bindings.get(instance.name)
        if not isinstance(binding, dict):
            fail("PROJECT_NOT_BOUND", f"aucun binding de déploiement pour {instance.name}")
        if binding != manifest:
            fail("PROJECT_BINDING_MISMATCH", "le manifeste et le binding hôte divergent", manifest=manifest, binding=binding)
        profile = profiles.get(manifest["deployment_profile"])
        if not isinstance(profile, dict):
            fail("DEPLOYMENT_PROFILE_MISSING", f"profil absent: {manifest['deployment_profile']}")
        return root, instance, manifest, profile

    def pid_path(self, instance: Instance) -> Path:
        return self.state_dir / f"{instance.name}.pid"

    def log_path(self, instance: Instance) -> Path:
        return self.state_dir / f"{instance.name}.log"

    def pid(self, instance: Instance) -> int | None:
        try:
            pid = int(self.pid_path(instance).read_text().strip())
            os.kill(pid, 0)
            return pid
        except (OSError, ValueError):
            return None

    @contextlib.contextmanager
    def lock(self, instance: Instance) -> Iterator[None]:
        path = self.state_dir / "locks" / f"{instance.name}.lock"
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a+") as stream:
            fcntl.flock(stream, fcntl.LOCK_EX)
            yield

    def host(self) -> str:
        return "127.0.0.1" if self.bind in {"", "0.0.0.0", "::"} else self.bind

    def url(self, instance: Instance) -> str:
        return f"http://{self.host()}:{instance.port}"

    @staticmethod
    def port_open(port: int) -> bool:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.4):
                return True
        except OSError:
            return False

    def state(self, instance: Instance) -> tuple[str, int | None]:
        pid = self.pid(instance)
        open_ = self.port_open(instance.port)
        if pid is not None:
            return ("up" if open_ else "starting"), pid
        return ("foreign" if open_ else "down"), None

    @staticmethod
    def http_json(url: str, path: str, timeout: float = 2.0) -> tuple[int | None, Any]:
        try:
            with urlopen(Request(url + path, headers={"Accept": "application/json"}), timeout=timeout) as response:
                raw = response.read()
                return response.status, json.loads(raw) if raw else None
        except HTTPError as exc:
            try:
                raw = exc.read()
                return exc.code, json.loads(raw)
            except (json.JSONDecodeError, UnicodeDecodeError):
                return exc.code, None
            finally:
                exc.close()
        except (URLError, TimeoutError, OSError, json.JSONDecodeError):
            return None, None

    @staticmethod
    def proc_env(pid: int) -> dict[str, str]:
        try:
            raw = Path(f"/proc/{pid}/environ").read_bytes()
        except OSError as exc:
            fail("PROCESS_INSPECTION_FAILED", f"environnement du pid {pid} illisible", cause=str(exc))
        env: dict[str, str] = {}
        for entry in raw.split(b"\0"):
            if b"=" not in entry:
                continue
            key, value = entry.split(b"=", 1)
            env[key.decode(errors="surrogateescape")] = value.decode(errors="surrogateescape")
        return env

    def store(self, root: Path, instance: Instance, pid: int | None) -> dict[str, Any]:
        explicit = self._flag_value(instance.extra, "--store-dir")
        if explicit is not None:
            path = Path(explicit)
            return {"path": str((root / path).resolve() if not path.is_absolute() else path.resolve()), "source": "instance-flag"}
        if instance.local_store:
            return {"path": str(root / ".iterion"), "source": "local-store"}
        iterion_home = None
        source = "engine-default"
        if pid is not None:
            iterion_home = self.proc_env(pid).get("ITERION_HOME")
            if iterion_home:
                source = "live-process-ITERION_HOME"
        elif os.environ.get("ITERION_HOME"):
            iterion_home = os.environ["ITERION_HOME"]
            source = "manager-ITERION_HOME"
        base = Path(iterion_home).expanduser() if iterion_home else Path.home() / ".iterion"
        key = "-" + str(root).replace("\\", "-").replace(":", "-").replace("/", "-").lstrip("-")
        return {"path": str((base / "projects" / key).resolve()), "source": source}

    @staticmethod
    def _flag_value(args: list[str], flag: str) -> str | None:
        for index, value in enumerate(args):
            if value == flag and index + 1 < len(args):
                return args[index + 1]
            if value.startswith(flag + "="):
                return value.split("=", 1)[1]
        return None

    def selected_binary(self, instance: Instance) -> Path:
        selectors = (
            self.state_dir / "deployments" / instance.name / "current.json",
            self.state_dir / "runtime" / instance.name / "current.json",
        )
        for selector in selectors:
            if selector.is_file():
                data = read_json(selector, "BINARY_SELECTOR_INVALID")
                path = canonical(data.get("binary", ""))
                if path.name != "iterion" or sha256_file(path) != data.get("sha256"):
                    fail("BINARY_SELECTOR_INVALID", f"artefact sélectionné invalide pour {instance.name}")
                return path
        candidate = self.configured_bin or shutil.which("iterion")
        if not candidate:
            fail("ITERION_BINARY_MISSING", "binaire iterion introuvable")
        path = canonical(candidate)
        if not os.access(path, os.X_OK):
            fail("ITERION_BINARY_MISSING", f"binaire non exécutable: {path}")
        return path

    def record_runtime_binary(self, instance: Instance, binary: Path, source: str) -> dict[str, Any]:
        receipt = {
            "schema_version": SCHEMA_VERSION,
            "instance": instance.name,
            "binary": str(binary),
            "sha256": sha256_file(binary),
            "source": source,
            "recorded_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        }
        atomic_json(self.state_dir / "runtime" / instance.name / "current.json", receipt)
        return receipt

    def adopt_active(self, names: list[str]) -> list[dict[str, Any]]:
        results: list[dict[str, Any]] = []
        for instance in self.targets(names):
            with self.lock(instance):
                state, pid = self.state(instance)
                if state != "up" or pid is None:
                    fail("INSTANCE_NOT_MANAGED_UP", f"[{instance.name}] adoption impossible: état {state}")
                self.verify_adoption(instance, pid, canonical(instance.project_dir))
                results.append(self._adopt_pid(instance, pid))
        return results

    def _adopt_pid(self, instance: Instance, pid: int) -> dict[str, Any]:
        adopted_root = self.state_dir / "adopted" / instance.name
        adopted_root.mkdir(parents=True, exist_ok=True)
        temporary = adopted_root / ".iterion.tmp"
        try:
            with Path(f"/proc/{pid}/exe").open("rb") as source, temporary.open("wb") as target:
                shutil.copyfileobj(source, target)
            digest = sha256_file(temporary)
            artifact = adopted_root / digest / "iterion"
            artifact.parent.mkdir(parents=True, exist_ok=True)
            if artifact.exists() and sha256_file(artifact) != digest:
                fail("ARTIFACT_COLLISION", f"collision pendant l'adoption de {instance.name}")
            if not artifact.exists():
                os.chmod(temporary, 0o555)
                os.replace(temporary, artifact)
            receipt = self.record_runtime_binary(instance, artifact, "adopted-live-process")
            return {"name": instance.name, "pid": pid, **receipt}
        finally:
            temporary.unlink(missing_ok=True)

    def adopt_process(self, project: str | None, pid: int) -> dict[str, Any]:
        root, instance, _manifest, _profile = self.resolve(project)
        with self.lock(instance):
            state, managed_pid = self.state(instance)
            if state != "foreign" or managed_pid is not None:
                fail("INSTANCE_NOT_FOREIGN", f"[{instance.name}] adoption externe impossible: état {state}")
            if not self.process_alive(pid):
                fail("PROCESS_NOT_RUNNING", f"pid inactif: {pid}")
            status, info = self.http_json(self.url(instance), "/api/server/info")
            if status != 200 or not isinstance(info, dict) or not info.get("work_dir"):
                fail("LEGACY_ADOPTION_UNCERTAIN", "le processus étranger n'expose pas server/info")
            if canonical(info["work_dir"]) != root:
                fail("LEGACY_ADOPTION_UNCERTAIN", "le processus étranger sert un autre projet", server_info=info)
            self.verify_adoption(instance, pid, root)
            receipt = self._adopt_pid(instance, pid)
            self.pid_path(instance).parent.mkdir(parents=True, exist_ok=True)
            self.pid_path(instance).write_text(str(pid))
            receipt["adopted_foreign_process"] = True
            return receipt

    def launch_args(self, instance: Instance, binary: Path, recovery_passive: bool = False, root: Path | None = None, store: Path | None = None, port: int | None = None) -> list[str]:
        project = root or instance.project_dir
        args = [str(binary), "studio", "--dir", str(project), "--port", str(port or instance.port), "--bind", self.bind, "--no-browser"]
        if instance.local_store:
            args += ["--store-dir", str(store or (project / ".iterion"))]
        args += instance.extra
        if recovery_passive:
            args += ["--no-browser-pane", "--recovery-passive"]
        return args

    def load_launch_env(self, instance: Instance, binary: Path) -> dict[str, str]:
        env = dict(os.environ)
        dotenv = instance.project_dir / ".env"
        if self.load_project_env and not instance.no_env and dotenv.is_file():
            script = 'set -a; . "$1"; set +a; env -0'
            try:
                raw = subprocess.check_output(["bash", "--noprofile", "--norc", "-c", script, "iterion-env", str(dotenv)])
            except (OSError, subprocess.CalledProcessError) as exc:
                fail("PROJECT_ENV_INVALID", f"impossible de charger {dotenv}", cause=str(exc))
            env = {}
            for entry in raw.split(b"\0"):
                if b"=" in entry:
                    key, value = entry.split(b"=", 1)
                    env[key.decode(errors="surrogateescape")] = value.decode(errors="surrogateescape")
        env["ITERION_BIN"] = str(binary)
        return env

    def spawn(self, instance: Instance, binary: Path, env: dict[str, str] | None = None) -> int:
        if self.port_open(instance.port):
            fail("PORT_ALREADY_IN_USE", f"port {instance.port} déjà occupé")
        self.state_dir.mkdir(parents=True, exist_ok=True)
        if instance.local_store:
            store = instance.project_dir / ".iterion"
            store.mkdir(parents=True, exist_ok=True)
            (store / ".iterion-store").touch(exist_ok=True)
        log_path = self.log_path(instance)
        launch_env = env or self.load_launch_env(instance, binary)
        launch_env["ITERION_BIN"] = str(binary)
        with log_path.open("ab", buffering=0) as log:
            log.write(f"\n===== {instance.name} | {instance.project_dir} | start {time.strftime('%Y-%m-%dT%H:%M:%S%z')} | binary {binary} =====\n".encode())
            process = subprocess.Popen(
                self.launch_args(instance, binary),
                cwd=instance.project_dir,
                env=launch_env,
                stdin=subprocess.DEVNULL,
                stdout=log,
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )
        self.pid_path(instance).write_text(str(process.pid))
        return process.pid

    def wait_ready(self, instance: Instance, pid: int, timeout: float = 40.0) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if not self.process_alive(pid):
                fail("INSTANCE_START_FAILED", f"{instance.name} s'est arrêté pendant le démarrage", log=str(self.log_path(instance)))
            status, info = self.http_json(self.url(instance), "/api/server/info")
            if status == 200 and isinstance(info, dict):
                return info
            time.sleep(0.25)
        fail("INSTANCE_START_TIMEOUT", f"{instance.name} ne répond pas après {timeout:.0f}s")

    @staticmethod
    def process_alive(pid: int) -> bool:
        try:
            os.kill(pid, 0)
            stat_fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
            if stat_fields and stat_fields[0] == "Z":
                return False
            return True
        except (OSError, IndexError):
            return False

    def stop_pid(self, instance: Instance, pid: int, timeout: float, allow_kill: bool) -> None:
        try:
            pgid = os.getpgid(pid)
            os.killpg(pgid, signal.SIGTERM) if pgid == pid else os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        deadline = time.monotonic() + timeout
        while self.process_alive(pid) and time.monotonic() < deadline:
            time.sleep(0.25)
        if self.process_alive(pid):
            if not allow_kill:
                fail("GRACEFUL_SHUTDOWN_TIMEOUT", f"{instance.name} n'a pas terminé après SIGTERM; aucun SIGKILL envoyé")
            os.kill(pid, signal.SIGKILL)
            deadline = time.monotonic() + 5
            while self.process_alive(pid) and time.monotonic() < deadline:
                time.sleep(0.1)
        self.pid_path(instance).unlink(missing_ok=True)

    def capability(self, instance: Instance, run_id: str | None) -> dict[str, Any]:
        if not run_id:
            return {"state": "not-probed", "http_status": None}
        status, payload = self.http_json(self.url(instance), f"/api/runs/{run_id}/assistant-missions")
        if status == 200:
            documented = isinstance(payload, list) or (isinstance(payload, dict) and isinstance(payload.get("missions"), list))
            return {"state": "present" if documented else "indeterminate", "http_status": status, "documented_list": documented}
        if status == 404:
            return {"state": "absent-recoverable", "http_status": status}
        if status in {401, 403}:
            return {"state": "unauthorized", "http_status": status}
        return {"state": "indeterminate", "http_status": status}

    def context_project(self, project: str | None, run_id: str | None) -> str:
        if project is not None:
            return project
        cwd = canonical(os.getcwd())
        cwd_matches = [instance for instance in self.instances if canonical(instance.project_dir) == cwd]
        if cwd_matches or not run_id:
            return str(cwd)
        candidates: list[Instance] = []
        for instance in self.instances:
            state, _pid = self.state(instance)
            if state != "up":
                continue
            run_status, _run = self.http_json(self.url(instance), f"/api/runs/{run_id}")
            info_status, info = self.http_json(self.url(instance), "/api/server/info")
            if run_status != 200 or info_status != 200 or not isinstance(info, dict) or not info.get("work_dir"):
                continue
            try:
                if canonical(info["work_dir"]) == canonical(instance.project_dir):
                    candidates.append(instance)
            except ManagerError:
                continue
        if len(candidates) != 1:
            fail(
                "RUN_PROJECT_NOT_UNIQUE",
                "le run doit appartenir à une seule instance configurée, active et gérée",
                run_id=run_id,
                matches=[instance.name for instance in candidates],
            )
        return str(candidates[0].project_dir)

    @staticmethod
    def summarize_run(payload: Any) -> dict[str, Any] | None:
        if not isinstance(payload, dict):
            return None
        run = payload.get("run")
        source = run if isinstance(run, dict) else payload
        keys = (
            "id", "run_id", "status", "state", "resumable", "can_resume",
            "end_reason", "failure_code", "outcome_seq", "rewindable",
        )
        summary = {key: source[key] for key in keys if key in source}
        diagnostic = payload.get("diagnostic")
        if isinstance(diagnostic, dict):
            diagnostic_keys = ("status", "outcome", "recoverable", "next_action", "node_id", "failure_code")
            summary["diagnostic"] = {key: diagnostic[key] for key in diagnostic_keys if key in diagnostic}
        return summary

    def context(self, project: str | None, run_id: str | None) -> dict[str, Any]:
        root, instance, manifest, profile = self.resolve(self.context_project(project, run_id))
        state, pid = self.state(instance)
        status, info = self.http_json(self.url(instance), "/api/server/info") if state in {"up", "foreign"} else (None, None)
        capability = self.capability(instance, run_id) if state == "up" else {"state": "not-probed", "http_status": None}
        run_status, run_payload = (self.http_json(self.url(instance), f"/api/runs/{run_id}") if run_id and state == "up" else (None, None))
        run_summary = self.summarize_run(run_payload)
        server = {"reachable": status == 200, "http_status": status, "info": info if isinstance(info, dict) else None}
        if isinstance(info, dict) and info.get("work_dir"):
            try:
                server["work_dir_matches"] = Path(info["work_dir"]).resolve(strict=True) == root
            except (OSError, RuntimeError):
                server["work_dir_matches"] = False
        launch = {
            "project_dir": str(root),
            "bind": self.bind,
            "port": instance.port,
            "local_store": instance.local_store,
            "load_project_env": self.load_project_env and not instance.no_env,
            "extra": instance.extra,
            "selected_binary": str(self.selected_binary(instance)),
        }
        live_binary = None
        if pid is not None:
            try:
                live_binary = str(Path(f"/proc/{pid}/exe").resolve(strict=True))
            except OSError:
                live_binary = None
        token_body = {
            "instance": instance.name,
            "state": state,
            "pid": pid,
            "launch": launch,
            "server_info": info,
            "capability": capability,
            "run_id": run_id,
            "run_http_status": run_status,
            "run": run_summary,
        }
        return {
            "schema_version": SCHEMA_VERSION,
            "project": {"root": str(root), **manifest},
            "instance": {"name": instance.name, "state": state, "pid": pid, "url": self.url(instance), "live_binary": live_binary, "launch": launch},
            "store": self.store(root, instance, pid),
            "engine": {
                "source_repository": str(Path(profile["source_repository"]).expanduser().resolve()),
                "integration_base_ref": profile["integration_base_ref"],
                "worktree_root": str(Path(profile["worktree_root"]).expanduser().resolve()),
                "build_output": profile["build_output"],
            },
            "server": server,
            "assistant_missions": capability,
            "run": {"id": run_id, "http_status": run_status, "data": run_summary},
            "context_token": json_hash(token_body),
        }

    def session_path(self, project_id: str, session: str) -> Path:
        return self.state_dir / "sessions" / project_id / f"{session}.json"

    def prepare(self, project: str | None, session: str) -> dict[str, Any]:
        if not SAFE_ID.fullmatch(session):
            fail("SESSION_ID_INVALID", "session doit contenir uniquement lettres, chiffres, point, tiret ou underscore")
        root, instance, manifest, profile = self.resolve(project)
        with self.lock(instance):
            record_path = self.session_path(manifest["project_id"], session)
            if record_path.is_file():
                record = read_json(record_path, "SESSION_STATE_INVALID")
                worktree = canonical(record.get("worktree", ""))
                if record.get("project_root") != str(root):
                    fail("SESSION_OWNERSHIP_MISMATCH", "cet identifiant de session appartient à un autre projet")
                record["reused"] = True
                return record
            repository = canonical(profile["source_repository"])
            base_ref = profile["integration_base_ref"]
            base_sha = run_checked(["git", "-C", str(repository), "rev-parse", f"{base_ref}^{{commit}}"])
            if not FULL_SHA.fullmatch(base_sha):
                fail("BASE_REF_INVALID", f"référence non résolue exactement: {base_ref}")
            worktree_root = Path(profile["worktree_root"]).expanduser().resolve()
            worktree = worktree_root / manifest["project_id"] / session
            branch = f"agent/bootstrap-{manifest['project_id']}-{session}"
            if worktree.exists():
                fail("WORKTREE_COLLISION", f"worktree déjà présent sans reçu: {worktree}")
            exists = subprocess.run(["git", "-C", str(repository), "show-ref", "--verify", "--quiet", f"refs/heads/{branch}"]).returncode == 0
            if exists:
                fail("BRANCH_COLLISION", f"branche déjà présente sans reçu: {branch}")
            worktree.parent.mkdir(parents=True, exist_ok=True)
            run_checked(["git", "-C", str(repository), "worktree", "add", "-b", branch, str(worktree), base_sha])
            record = {
                "schema_version": SCHEMA_VERSION,
                "session": session,
                "owner_uid": os.getuid(),
                "project_id": manifest["project_id"],
                "project_root": str(root),
                "instance": instance.name,
                "source_repository": str(repository),
                "integration_base_ref": base_ref,
                "base_sha": base_sha,
                "branch": branch,
                "worktree": str(worktree.resolve(strict=True)),
                "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "reused": False,
            }
            atomic_json(record_path, record)
            return record

    def build(self, project: str | None, session: str, commit: str) -> dict[str, Any]:
        if not FULL_SHA.fullmatch(commit):
            fail("COMMIT_NOT_EXACT", "--commit doit être un SHA Git complet de 40 caractères")
        root, instance, manifest, _profile = self.resolve(project)
        with self.lock(instance):
            record = read_json(self.session_path(manifest["project_id"], session), "SESSION_NOT_PREPARED")
            if record.get("project_root") != str(root) or record.get("owner_uid") != os.getuid():
                fail("SESSION_OWNERSHIP_MISMATCH", "session non possédée par ce projet/utilisateur")
            worktree = canonical(record.get("worktree", ""))
            head = run_checked(["git", "-C", str(worktree), "rev-parse", "HEAD^{commit}"])
            if head != commit:
                fail("COMMIT_HEAD_MISMATCH", "le SHA demandé n'est pas HEAD du worktree", head=head, requested=commit)
            dirty = run_checked(["git", "-C", str(worktree), "status", "--porcelain", "--untracked-files=all"])
            if dirty:
                fail("WORKTREE_NOT_CLEAN", "build refusé: le worktree contient des changements non committés", status=dirty.splitlines()[:50])
            for forbidden in (".env", ".task"):
                if (worktree / forbidden).exists():
                    fail("BUILD_NOT_HERMETIC", f"build refusé: {forbidden} existe dans le worktree")
            versions = {
                "python": sys.version.split()[0],
                "devbox": run_checked(["devbox", "version"]),
                "task": run_checked(["devbox", "run", "--", "task", "--version"], cwd=worktree),
                "go": run_checked(["devbox", "run", "--", "go", "version"], cwd=worktree),
            }
            output = worktree / "iterion"
            env = dict(os.environ)
            env["ITERION_BIN"] = str(output)
            run_checked(["devbox", "run", "--", "task", "build"], cwd=worktree, env=env)
            if not output.is_file() or output.name != "iterion" or not os.access(output, os.X_OK):
                fail("BUILD_OUTPUT_INVALID", f"artefact iterion absent ou non exécutable: {output}")
            embedded = run_checked([str(output), "version", "--commit"])
            if not commit.startswith(embedded):
                fail("BUILD_PROVENANCE_MISMATCH", "le commit embarqué ne correspond pas au SHA demandé", embedded=embedded, commit=commit)
            self.preflight(output)
            digest = sha256_file(output)
            artifact_id = f"{commit[:12]}-{digest[:16]}"
            artifact_dir = self.state_dir / "artifacts" / artifact_id
            artifact = artifact_dir / "iterion"
            artifact_dir.mkdir(parents=True, exist_ok=True)
            if artifact.exists() and sha256_file(artifact) != digest:
                fail("ARTIFACT_COLLISION", f"collision d'artefact: {artifact_id}")
            if not artifact.exists():
                temporary = artifact_dir / ".iterion.tmp"
                shutil.copyfile(output, temporary)
                os.chmod(temporary, 0o555)
                os.replace(temporary, artifact)
            metadata = {
                "schema_version": SCHEMA_VERSION,
                "artifact_id": artifact_id,
                "binary": str(artifact),
                "binary_basename": "iterion",
                "sha256": digest,
                "commit": commit,
                "embedded_commit": embedded,
                "session": session,
                "worktree": str(worktree),
                "branch": record["branch"],
                "base_sha": record["base_sha"],
                "tool_versions": versions,
                "preflight": {"isolated": True, "recovery_passive": True},
                "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            }
            atomic_json(artifact_dir / "metadata.json", metadata)
            return metadata

    def preflight(self, binary: Path) -> None:
        temporary_root = self.state_dir / "tmp"
        temporary_root.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="iterion-preflight-", dir=temporary_root) as temporary:
            temp = Path(temporary)
            project = temp / "project"
            store = temp / "store"
            project.mkdir()
            store.mkdir()
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                port = reservation.getsockname()[1]
            log_path = temp / "studio.log"
            env = dict(os.environ)
            env["ITERION_BIN"] = str(binary)
            args = [str(binary), "studio", "--dir", str(project), "--store-dir", str(store), "--bind", "127.0.0.1", "--port", str(port), "--no-browser", "--no-browser-pane", "--recovery-passive"]
            with log_path.open("wb") as log:
                process = subprocess.Popen(args, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            try:
                deadline = time.monotonic() + 40
                info = None
                while time.monotonic() < deadline and process.poll() is None:
                    status, candidate = self.http_json(f"http://127.0.0.1:{port}", "/api/server/info")
                    if status == 200 and isinstance(candidate, dict):
                        info = candidate
                        break
                    time.sleep(0.25)
                if not info:
                    fail("ARTIFACT_PREFLIGHT_FAILED", "le Studio isolé n'est pas devenu prêt", log=log_path.read_text(errors="replace")[-4000:])
                if Path(info.get("work_dir", "")).resolve() != project.resolve() or info.get("recovery_passive") is not True:
                    fail("ARTIFACT_PREFLIGHT_FAILED", "le préflight n'atteste pas work_dir et recovery_passive", server_info=info)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGTERM)
                    try:
                        process.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        os.killpg(process.pid, signal.SIGKILL)
                        process.wait(timeout=5)

    def active_runs(self, instance: Instance) -> list[dict[str, Any]]:
        status, payload = self.http_json(self.url(instance), "/api/runs")
        if status != 200:
            fail("RUN_INVENTORY_UNAVAILABLE", "impossible d'énumérer les runs avant déploiement", http_status=status)
        if isinstance(payload, dict):
            candidates = payload.get("runs") or payload.get("items") or []
        else:
            candidates = payload
        if not isinstance(candidates, list):
            fail("RUN_INVENTORY_INVALID", "la réponse /api/runs n'est pas une liste documentée")
        return [item for item in candidates if isinstance(item, dict)]

    @staticmethod
    def run_id(item: dict[str, Any]) -> str:
        return str(item.get("id") or item.get("run_id") or "")

    @staticmethod
    def run_state(item: dict[str, Any]) -> str:
        return str(item.get("status") or item.get("state") or "").lower()

    def verify_adoption(self, instance: Instance, pid: int, root: Path) -> None:
        try:
            args = [part.decode(errors="surrogateescape") for part in Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0") if part]
            executable = Path(f"/proc/{pid}/exe").resolve(strict=True)
        except OSError as exc:
            fail("LEGACY_ADOPTION_UNCERTAIN", "impossible d'inspecter le processus existant", cause=str(exc))
        if not executable.name.startswith("iterion") or "studio" not in args:
            fail("LEGACY_ADOPTION_UNCERTAIN", "le pid géré n'est pas un Studio iterion", argv=args[:12])
        expected_args = self.launch_args(instance, executable)[1:]
        if args[1:] != expected_args:
            fail("LEGACY_ADOPTION_UNCERTAIN", "la ligne de commande live diverge de la recette hôte", expected=expected_args, actual=args[1:])
        if canonical(self._flag_value(args, "--dir") or "") != root:
            fail("LEGACY_ADOPTION_UNCERTAIN", "le chemin réel live diverge de la recette hôte")

    def freeze_runtime(self, instance: Instance, pid: int, transaction_dir: Path) -> dict[str, Any]:
        previous = transaction_dir / "previous" / "iterion"
        previous.parent.mkdir(parents=True, exist_ok=True)
        try:
            with Path(f"/proc/{pid}/exe").open("rb") as source, previous.open("wb") as target:
                shutil.copyfileobj(source, target)
            os.chmod(previous, 0o555)
        except OSError as exc:
            fail("LEGACY_ADOPTION_UNCERTAIN", "impossible de figer l'ancien binaire", cause=str(exc))
        env_path = transaction_dir / "previous" / "environment.json"
        try:
            raw_env = Path(f"/proc/{pid}/environ").read_bytes()
        except OSError as exc:
            fail("PROCESS_INSPECTION_FAILED", f"environnement du pid {pid} illisible", cause=str(exc))
        atomic_json(env_path, {
            "schema_version": SCHEMA_VERSION,
            "format": "nul-base64-v1",
            "data": base64.b64encode(raw_env).decode("ascii"),
        }, mode=0o600)
        return {"binary": str(previous), "sha256": sha256_file(previous), "environment_file": str(env_path)}

    @staticmethod
    def load_frozen_env(path: Path) -> dict[str, str]:
        envelope = read_json(path, "ROLLBACK_STATE_INVALID")
        if envelope.get("schema_version") != SCHEMA_VERSION or envelope.get("format") != "nul-base64-v1" or not isinstance(envelope.get("data"), str):
            fail("ROLLBACK_STATE_INVALID", f"snapshot d'environnement invalide: {path}")
        try:
            raw = base64.b64decode(envelope["data"], validate=True)
        except (ValueError, TypeError) as exc:
            fail("ROLLBACK_STATE_INVALID", f"snapshot d'environnement corrompu: {path}", cause=str(exc))
        env: dict[str, str] = {}
        for entry in raw.split(b"\0"):
            if b"=" in entry:
                key, value = entry.split(b"=", 1)
                env[key.decode(errors="surrogateescape")] = value.decode(errors="surrogateescape")
        return env

    def journal(self, path: Path, transaction: dict[str, Any], phase: str, **updates: Any) -> None:
        transaction.update(updates)
        transaction["phase"] = phase
        transaction["updated_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        atomic_json(path, transaction)

    def deploy(self, project: str | None, artifact_id: str, expect_context: str, goal: str, run_id: str) -> dict[str, Any]:
        if not AUTHORITY_ID.fullmatch(goal) or not SAFE_ID.fullmatch(run_id):
            fail("DEPLOY_AUTHORITY_INVALID", "--goal et --run doivent être des identifiants explicites")
        root, instance, manifest, _profile = self.resolve(project)
        with self.lock(instance):
            context = self.context(str(root), run_id)
            if context["context_token"] != expect_context:
                fail("CONTEXT_CHANGED", "le contexte a changé depuis le contrôle; relancer context", current_context_token=context["context_token"])
            if context["instance"]["state"] != "up" or not context["server"]["reachable"]:
                fail("INSTANCE_NOT_MANAGED_UP", "le bootstrap ne peut remplacer qu'une instance déjà active et gérée")
            if context["server"].get("work_dir_matches") is not True:
                fail("LIVE_PROJECT_MISMATCH", "le Studio live ne sert pas le chemin réel attendu")
            if context["assistant_missions"]["state"] != "absent-recoverable":
                fail("BOOTSTRAP_NOT_NEEDED", "le bootstrap exige un adaptateur assistant-missions absent avec HTTP 404", capability=context["assistant_missions"])
            target = context["run"].get("data") or {}
            target_state = self.run_state(target)
            if any(marker in target_state for marker in HUMAN_GATE_MARKERS):
                fail("HUMAN_GATE_ACTIVE", "un gate humain/opérateur est actif; aucun redéploiement automatique")
            runs = self.active_runs(instance)
            non_terminal = [item for item in runs if self.run_state(item) not in TERMINAL_RUN_STATES]
            others = [item for item in non_terminal if self.run_id(item) != run_id]
            if others:
                fail("OTHER_ACTIVE_RUNS", "d'autres runs non terminaux utilisent cette instance", runs=[{"id": self.run_id(item), "status": self.run_state(item)} for item in others])
            if target_state and target_state not in TERMINAL_RUN_STATES:
                resumable = bool(target.get("resumable") or target.get("can_resume") or "resumable" in target_state)
                if not resumable:
                    fail("TARGET_RUN_NOT_RESUMABLE", "le run cible est actif sans preuve de reprise sûre", status=target_state)
            metadata = read_json(self.state_dir / "artifacts" / artifact_id / "metadata.json", "ARTIFACT_NOT_FOUND")
            artifact = canonical(metadata.get("binary", ""))
            if artifact.name != "iterion" or sha256_file(artifact) != metadata.get("sha256") or metadata.get("preflight", {}).get("recovery_passive") is not True:
                fail("ARTIFACT_INVALID", "artefact non scellé ou préflight absent")
            pid = context["instance"]["pid"]
            assert isinstance(pid, int)
            self.verify_adoption(instance, pid, root)
            transaction_id = f"{int(time.time())}-{artifact_id}"
            transaction_dir = self.state_dir / "deployments" / instance.name / "transactions" / transaction_id
            journal_path = transaction_dir / "journal.json"
            transaction = {
                "schema_version": SCHEMA_VERSION,
                "transaction_id": transaction_id,
                "project_id": manifest["project_id"],
                "project_root": str(root),
                "instance": instance.name,
                "goal": goal,
                "run_id": run_id,
                "artifact_id": artifact_id,
                "candidate": {"binary": str(artifact), "sha256": metadata["sha256"], "commit": metadata["commit"]},
                "context_token": expect_context,
                "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            }
            self.journal(journal_path, transaction, "adopting-live-runtime")
            previous = self.freeze_runtime(instance, pid, transaction_dir)
            self.journal(journal_path, transaction, "previous-runtime-frozen", previous=previous)
            old_env = self.load_frozen_env(Path(previous["environment_file"]))
            try:
                self.stop_pid(instance, pid, timeout=90, allow_kill=False)
                self.journal(journal_path, transaction, "old-stopped")
                candidate_pid = self.spawn(instance, artifact, env={str(k): str(v) for k, v in old_env.items()})
                self.journal(journal_path, transaction, "candidate-starting", candidate_pid=candidate_pid)
                info = self.wait_ready(instance, candidate_pid)
                if Path(info.get("work_dir", "")).resolve(strict=True) != root or info.get("recovery_passive") is not False:
                    fail("LIVE_VERIFICATION_FAILED", "server/info ne confirme pas work_dir et recovery_passive=false", server_info=info)
                embedded = str(info.get("commit", ""))
                if not metadata["commit"].startswith(embedded):
                    fail("LIVE_VERIFICATION_FAILED", "le commit live ne correspond pas à l'artefact", expected=metadata["commit"], actual=embedded)
                capability = self.capability(instance, run_id)
                if capability["state"] != "present":
                    fail("LIVE_VERIFICATION_FAILED", "assistant-missions n'est pas disponible après le switch", capability=capability)
                selector = {"schema_version": SCHEMA_VERSION, "artifact_id": artifact_id, "binary": str(artifact), "sha256": metadata["sha256"], "commit": metadata["commit"]}
                atomic_json(self.state_dir / "deployments" / instance.name / "current.json", selector)
                self.record_runtime_binary(instance, artifact, "bootstrap-deployment")
                self.journal(journal_path, transaction, "committed", server_info=info, capability=capability)
                return {"deployed": True, "rolled_back": False, "transaction": transaction, "journal": str(journal_path)}
            except ManagerError as deployment_error:
                self.journal(journal_path, transaction, "rollback-starting", deployment_error={"code": deployment_error.code, "message": deployment_error.message})
                candidate_pid = self.pid(instance)
                if candidate_pid is not None:
                    self.stop_pid(instance, candidate_pid, timeout=45, allow_kill=False)
                previous_binary = canonical(previous["binary"])
                rollback_pid = self.spawn(instance, previous_binary, env={str(k): str(v) for k, v in old_env.items()})
                rollback_info = self.wait_ready(instance, rollback_pid)
                self.record_runtime_binary(instance, previous_binary, "deployment-rollback")
                self.journal(journal_path, transaction, "rolled-back", rollback_pid=rollback_pid, rollback_server_info=rollback_info)
                fail("DEPLOYMENT_ROLLED_BACK", "le candidat a échoué; l'ancien binaire exact a été relancé", journal=str(journal_path), deployment_error=deployment_error.code)

    def deployment_status(self, project: str | None) -> dict[str, Any]:
        root, instance, manifest, _profile = self.resolve(project)
        deployment_dir = self.state_dir / "deployments" / instance.name
        selector = read_json(deployment_dir / "current.json", "BINARY_SELECTOR_INVALID") if (deployment_dir / "current.json").is_file() else None
        journals: list[dict[str, Any]] = []
        transactions = deployment_dir / "transactions"
        if transactions.is_dir():
            for path in sorted(transactions.glob("*/journal.json"), reverse=True)[:10]:
                journals.append(read_json(path, "DEPLOYMENT_JOURNAL_INVALID"))
        return {"schema_version": SCHEMA_VERSION, "project": {"root": str(root), **manifest}, "instance": instance.name, "selected": selector, "transactions": journals}

    def start(self, names: list[str]) -> list[dict[str, Any]]:
        results = []
        for instance in self.targets(names):
            with self.lock(instance):
                state, pid = self.state(instance)
                if state in {"up", "starting"}:
                    results.append({"name": instance.name, "state": state, "pid": pid, "url": self.url(instance), "changed": False})
                    continue
                if state == "foreign":
                    fail("PORT_ALREADY_IN_USE", f"[{instance.name}] port {instance.port} occupé par un processus étranger")
                binary = self.selected_binary(instance)
                pid = self.spawn(instance, binary)
                self.wait_ready(instance, pid)
                self.record_runtime_binary(instance, binary, "manager-start")
                results.append({"name": instance.name, "state": "up", "pid": pid, "url": self.url(instance), "changed": True, "binary": str(binary)})
        return results

    def stop(self, names: list[str]) -> list[dict[str, Any]]:
        results = []
        for instance in self.targets(names):
            with self.lock(instance):
                pid = self.pid(instance)
                if pid is None:
                    self.pid_path(instance).unlink(missing_ok=True)
                    results.append({"name": instance.name, "state": "down", "changed": False})
                    continue
                self.stop_pid(instance, pid, timeout=15, allow_kill=True)
                results.append({"name": instance.name, "state": "down", "pid": pid, "changed": True})
        return results

    def restart(self, names: list[str]) -> list[dict[str, Any]]:
        targets = self.targets(names)
        self.stop([item.name for item in targets])
        return self.start([item.name for item in targets])

    def targets(self, names: list[str]) -> list[Instance]:
        if not names or names == ["all"]:
            return self.instances
        index = {item.name: item for item in self.instances}
        unknown = [name for name in names if name not in index]
        if unknown:
            fail("INSTANCE_UNKNOWN", f"instances inconnues: {', '.join(unknown)}")
        return [index[name] for name in names]


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(prog="iterion-instances", description="Gestionnaire local d'instances Iterion par projet")
    commands = root.add_subparsers(dest="command")

    def add_json(command: argparse.ArgumentParser) -> None:
        command.add_argument("--json", action="store_true")

    for name in ("start", "stop", "restart"):
        command = commands.add_parser(name)
        command.add_argument("names", nargs="*")
        add_json(command)
    command = commands.add_parser("adopt-active")
    command.add_argument("names", nargs="*")
    add_json(command)
    command = commands.add_parser("adopt-process")
    command.add_argument("--project")
    command.add_argument("--pid", type=int, required=True)
    add_json(command)
    add_json(commands.add_parser("status"))
    command = commands.add_parser("open")
    command.add_argument("names", nargs="+")
    add_json(command)
    command = commands.add_parser("logs")
    command.add_argument("name")
    command.add_argument("-f", "--follow", action="store_true")
    add_json(command)
    add_json(commands.add_parser("list"))
    add_json(commands.add_parser("config"))
    add_json(commands.add_parser("init"))
    command = commands.add_parser("context")
    command.add_argument("--project")
    command.add_argument("--run")
    add_json(command)
    command = commands.add_parser("prepare")
    command.add_argument("--project")
    command.add_argument("--session", required=True)
    add_json(command)
    command = commands.add_parser("build")
    command.add_argument("--project")
    command.add_argument("--session", required=True)
    command.add_argument("--commit", required=True)
    add_json(command)
    command = commands.add_parser("deploy")
    command.add_argument("--project")
    command.add_argument("--artifact", required=True)
    command.add_argument("--expect-context", required=True)
    command.add_argument("--goal", required=True)
    command.add_argument("--run", required=True)
    add_json(command)
    command = commands.add_parser("deployment-status")
    command.add_argument("--project")
    add_json(command)
    return root


def render_human(command: str, value: Any) -> None:
    if command == "status":
        print(f"{'NAME':10} {'PORT':6} {'PID':8} {'STATE':9} {'URL':28} DIR")
        for item in value:
            print(f"{item['name']:10} {item['port']:<6} {str(item.get('pid') or '-'):<8} {item['state']:<9} {item['url']:28} {item['project_dir']}")
    elif command == "list":
        print("\n".join(value))
    elif command == "config":
        print(value)
    elif isinstance(value, (dict, list)):
        print(json.dumps(value, indent=2, sort_keys=True))
    else:
        print(value)


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    command = args.command or "status"
    json_output = bool(getattr(args, "json", False))
    try:
        manager = Manager()
        if command == "status":
            value = [{"name": item.name, "port": item.port, "pid": manager.state(item)[1], "state": manager.state(item)[0], "url": manager.url(item), "project_dir": str(item.project_dir)} for item in manager.instances]
        elif command == "list":
            value = [item.name for item in manager.instances]
        elif command == "config":
            value = str(manager.config_path)
        elif command == "start":
            value = manager.start(args.names)
        elif command == "stop":
            value = manager.stop(args.names)
        elif command == "restart":
            value = manager.restart(args.names)
        elif command == "adopt-active":
            value = manager.adopt_active(args.names)
        elif command == "adopt-process":
            value = manager.adopt_process(args.project, args.pid)
        elif command == "open":
            value = manager.start(args.names)
            for item in value:
                webbrowser.open(item["url"])
        elif command == "logs":
            instance = manager.targets([args.name])[0]
            if args.follow:
                os.execvp("tail", ["tail", "-n", "200", "-f", str(manager.log_path(instance))])
            value = manager.log_path(instance).read_text(errors="replace").splitlines()[-200:]
        elif command == "init":
            fail("INIT_REQUIRES_MANUAL_CONFIG", f"créez explicitement {manager.config_path} et {manager.profiles_path}")
        elif command == "context":
            value = manager.context(args.project, args.run)
        elif command == "prepare":
            value = manager.prepare(args.project, args.session)
        elif command == "build":
            value = manager.build(args.project, args.session, args.commit)
        elif command == "deploy":
            value = manager.deploy(args.project, args.artifact, args.expect_context, args.goal, args.run)
        elif command == "deployment-status":
            value = manager.deployment_status(args.project)
        else:
            fail("COMMAND_UNKNOWN", f"commande inconnue: {command}")
        if json_output:
            print(json.dumps({"ok": True, "result": value}, sort_keys=True))
        else:
            render_human(command, value)
        return 0
    except ManagerError as exc:
        payload = {"ok": False, "error": {"code": exc.code, "message": exc.message, "details": exc.details}}
        if json_output:
            print(json.dumps(payload, sort_keys=True))
        else:
            print(f"iterion-instances: {exc.code}: {exc.message}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
