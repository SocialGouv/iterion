#!/usr/bin/env python3
"""Control the single per-user Iterion workspace service.

The durable desktop registry is the source of project identity.  This module
never starts one process per project and never edits the registry.
"""

from __future__ import annotations

import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
import time
from typing import Any, Callable, Iterator, Sequence
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode, urlsplit
from urllib.request import Request, urlopen
import webbrowser


SCHEMA_VERSION = 1
SERVICE_NAME = "iterion-workspace.service"
DEFAULT_ORIGIN = "http://127.0.0.1:4891"
ACTIVE_RUN_STATES = ("running", "queued")
COMMAND_TIMEOUT = 15
HTTP_TIMEOUT = 5
READY_TIMEOUT = 20
RETIRED_COMMANDS = {
    "adopt-active",
    "adopt-process",
    "init",
    "prepare",
    "build",
    "deploy",
    "deployment-status",
}


class WorkspaceError(Exception):
    """A stable controller error suitable for human and JSON output."""

    def __init__(self, code: str, message: str, **details: Any):
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details


def fail(code: str, message: str, **details: Any) -> None:
    raise WorkspaceError(code, message, **details)


def registry_path() -> Path:
    config_home = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config"))
    return config_home / "Iterion" / "config.json"


def workspace_origin() -> str:
    raw = os.environ.get("ITERION_WORKSPACE_URL", DEFAULT_ORIGIN).strip().rstrip("/")
    parsed = urlsplit(raw)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        fail("WORKSPACE_URL_INVALID", "ITERION_WORKSPACE_URL must be an HTTP(S) origin", value=raw)
    if parsed.path not in {"", "/"} or parsed.query or parsed.fragment:
        fail("WORKSPACE_URL_INVALID", "ITERION_WORKSPACE_URL must not include a path, query, or fragment", value=raw)
    return raw


def load_registry(path: Path | None = None) -> dict[str, Any]:
    source = path or registry_path()
    try:
        data = json.loads(source.read_text(encoding="utf-8"))
    except FileNotFoundError:
        fail("REGISTRY_MISSING", f"Iterion project registry not found: {source}", path=str(source))
    except (OSError, json.JSONDecodeError) as exc:
        fail("REGISTRY_INVALID", f"Cannot read Iterion project registry: {exc}", path=str(source))
    if not isinstance(data, dict) or not isinstance(data.get("recent_projects"), list):
        fail("REGISTRY_INVALID", "Iterion project registry has an invalid shape", path=str(source))
    if data.get("version") not in {None, 1}:
        fail("REGISTRY_UNSUPPORTED", "Unsupported Iterion project registry version", version=data.get("version"))
    seen: set[str] = set()
    for project in data["recent_projects"]:
        if not isinstance(project, dict):
            fail("REGISTRY_INVALID", "A registry project entry is not an object", path=str(source))
        project_id = project.get("id")
        if not isinstance(project_id, str) or not project_id or project_id in seen:
            fail("REGISTRY_INVALID", "A registry project id is missing or duplicated", project_id=project_id)
        if not isinstance(project.get("dir"), str) or not project["dir"]:
            fail("REGISTRY_INVALID", "A registry project directory is missing", project_id=project_id)
        seen.add(project_id)
    return data


def project_url(project_id: str, origin: str | None = None) -> str:
    return f"{(origin or workspace_origin()).rstrip('/')}/x/{quote(project_id, safe='')}/"


def _normalized_paths(raw: str) -> set[Path]:
    expanded = Path(raw).expanduser()
    try:
        absolute = Path(os.path.abspath(expanded))
    except OSError:
        absolute = expanded.absolute()
    paths = {absolute}
    try:
        paths.add(absolute.resolve(strict=False))
    except OSError:
        pass
    return paths


def _path_contains(root: Path, candidate: Path) -> bool:
    try:
        candidate.relative_to(root)
        return True
    except ValueError:
        return False


def resolve_project(
    registry: dict[str, Any],
    selector: str | None = None,
    *,
    cwd: Path | None = None,
    use_current: bool = False,
) -> dict[str, Any]:
    projects = registry["recent_projects"]
    if selector and use_current:
        fail("PROJECT_SELECTOR_CONFLICT", "Use either --project or --current, not both")
    if use_current:
        current_id = registry.get("current_project_id")
        matches = [p for p in projects if p.get("id") == current_id]
        if len(matches) != 1:
            fail("CURRENT_PROJECT_UNAVAILABLE", "The registry current project is unavailable", project_id=current_id)
        return matches[0]
    if selector:
        id_matches = [p for p in projects if p.get("id") == selector]
        if id_matches:
            return id_matches[0]
        name_matches = [p for p in projects if p.get("name") == selector]
        if len(name_matches) == 1:
            return name_matches[0]
        if len(name_matches) > 1:
            fail("PROJECT_AMBIGUOUS", f"Several registered projects are named {selector!r}")
        target_raw = selector
    else:
        target_raw = str(cwd or Path.cwd())

    target_paths = _normalized_paths(target_raw)
    matches: list[tuple[int, dict[str, Any]]] = []
    for project in projects:
        roots = _normalized_paths(project["dir"])
        if any(_path_contains(root, target) for root in roots for target in target_paths):
            depth = max(len(root.parts) for root in roots)
            matches.append((depth, project))
    if not matches:
        fail(
            "PROJECT_NOT_REGISTERED",
            f"No registered Iterion project contains {target_raw}",
            selector=target_raw,
        )
    best_depth = max(depth for depth, _ in matches)
    best = [project for depth, project in matches if depth == best_depth]
    ids = {project["id"] for project in best}
    if len(ids) != 1:
        fail("PROJECT_AMBIGUOUS", f"Several registered projects match {target_raw}", project_ids=sorted(ids))
    return best[0]


def run_process(argv: Sequence[str], *, timeout: int | None = COMMAND_TIMEOUT) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            list(argv),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=timeout,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        fail("COMMAND_FAILED", f"Cannot execute {argv[0]}: {exc}", argv=list(argv))


def fetch_json(url: str, *, timeout: int = HTTP_TIMEOUT) -> Any:
    request = Request(url, headers={"Accept": "application/json"})
    try:
        with urlopen(request, timeout=timeout) as response:
            raw = response.read()
    except HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")[:300]
        fail("HTTP_FAILED", f"Workspace returned HTTP {exc.code}", url=url, status=exc.code, detail=detail)
    except (URLError, OSError) as exc:
        fail("WORKSPACE_UNREACHABLE", f"Workspace is unreachable: {exc}", url=url)
    try:
        return json.loads(raw)
    except json.JSONDecodeError as exc:
        fail("HTTP_RESPONSE_INVALID", f"Workspace returned invalid JSON: {exc}", url=url)


def lock_path() -> Path:
    runtime = os.environ.get("XDG_RUNTIME_DIR", "").strip()
    if runtime:
        candidate = Path(runtime)
        if candidate.is_dir():
            return candidate / "iterion-workspacectl.lock"
    return Path("/tmp") / f"iterion-workspacectl-{os.getuid()}.lock"


@contextlib.contextmanager
def mutation_lock(path: Path | None = None) -> Iterator[None]:
    target = path or lock_path()
    target.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(target, os.O_CREAT | os.O_RDWR, 0o600)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "a+", encoding="utf-8") as handle:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
            yield
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
    except Exception:
        try:
            os.close(fd)
        except OSError:
            pass
        raise


class Controller:
    def __init__(
        self,
        *,
        registry_file: Path | None = None,
        origin: str | None = None,
        runner: Callable[..., subprocess.CompletedProcess[str]] = run_process,
        fetcher: Callable[..., Any] = fetch_json,
        opener: Callable[[str], bool] = webbrowser.open,
        sleeper: Callable[[float], None] = time.sleep,
    ):
        self.registry_file = registry_file or registry_path()
        self.origin = (origin or workspace_origin()).rstrip("/")
        self.runner = runner
        self.fetcher = fetcher
        self.opener = opener
        self.sleeper = sleeper

    def registry(self) -> dict[str, Any]:
        return load_registry(self.registry_file)

    def service_state(self) -> str:
        result = self.runner(["systemctl", "--user", "is-active", SERVICE_NAME], timeout=COMMAND_TIMEOUT)
        state = result.stdout.strip()
        if state:
            return state
        return "inactive" if result.returncode else "unknown"

    def _systemctl(self, action: str) -> None:
        result = self.runner(["systemctl", "--user", action, SERVICE_NAME], timeout=COMMAND_TIMEOUT)
        if result.returncode != 0:
            fail(
                "SYSTEMD_FAILED",
                f"systemctl {action} failed for {SERVICE_NAME}",
                action=action,
                stderr=result.stderr.strip()[-500:],
            )

    def _project_view(self, project: dict[str, Any], runtime: dict[str, Any] | None = None) -> dict[str, Any]:
        view = {
            "id": project["id"],
            "name": project.get("name") or Path(project["dir"]).name,
            "dir": project["dir"],
            "store_dir": project.get("store_dir") or "",
            "bots_paths": project.get("bots_paths") or [],
            "env_file": project.get("env_file") or "",
            "last_opened": project.get("last_opened") or "",
            "url": project_url(project["id"], self.origin),
            "state": "unknown",
            "runtime_ready": False,
        }
        if runtime:
            view["state"] = runtime.get("state", "unknown")
            view["runtime_ready"] = bool(runtime.get("runtime_ready"))
            if runtime.get("error"):
                view["error"] = runtime["error"]
        elif not project.get("store_dir"):
            view["state"] = "degraded"
        return view

    def runtimes(self) -> list[dict[str, Any]]:
        payload = self.fetcher(f"{self.origin}/api/workspace/runtimes", timeout=HTTP_TIMEOUT)
        if not isinstance(payload, list) or not all(isinstance(item, dict) for item in payload):
            fail("HTTP_RESPONSE_INVALID", "Workspace runtimes response is not a list")
        return payload

    def list_projects(self) -> dict[str, Any]:
        registry = self.registry()
        runtimes_by_id: dict[str, dict[str, Any]] = {}
        reachable = False
        try:
            runtimes_by_id = {item.get("id"): item for item in self.runtimes() if isinstance(item.get("id"), str)}
            reachable = True
        except WorkspaceError:
            pass
        return {
            "registry_file": str(self.registry_file),
            "current_project_id": registry.get("current_project_id") or "",
            "service_reachable": reachable,
            "projects": [self._project_view(project, runtimes_by_id.get(project["id"])) for project in registry["recent_projects"]],
        }

    def context(self, selector: str | None, *, use_current: bool = False, cwd: Path | None = None) -> dict[str, Any]:
        registry = self.registry()
        project = resolve_project(registry, selector, cwd=cwd, use_current=use_current)
        runtime = None
        try:
            runtime = next((item for item in self.runtimes() if item.get("id") == project["id"]), None)
        except WorkspaceError:
            pass
        return {
            "service": {"name": SERVICE_NAME, "origin": self.origin, "state": self.service_state()},
            "project": self._project_view(project, runtime),
            "current_project_id": registry.get("current_project_id") or "",
        }

    def status(self) -> dict[str, Any]:
        state = self.service_state()
        active = state == "active"
        result: dict[str, Any] = {
            "service": {
                "name": SERVICE_NAME,
                "origin": self.origin,
                "state": state,
                "active": active,
                "reachable": False,
                "ready": False,
            },
            **self.list_projects(),
        }
        if active:
            try:
                ready = self.fetcher(f"{self.origin}/readyz", timeout=HTTP_TIMEOUT)
                result["service"].update(
                    {
                        "reachable": True,
                        "ready": isinstance(ready, dict) and not ready.get("degraded_project_ids") and int(ready.get("ready", 0)) > 0,
                        "ready_projects": ready.get("ready", 0) if isinstance(ready, dict) else 0,
                        "degraded_project_ids": ready.get("degraded_project_ids", []) if isinstance(ready, dict) else [],
                    }
                )
            except WorkspaceError as exc:
                result["service"]["error"] = {"code": exc.code, "message": exc.message}
        return result

    def _active_work(self) -> dict[str, list[dict[str, Any]]]:
        registry = self.registry()
        active: list[dict[str, Any]] = []
        errors: list[dict[str, Any]] = []
        for project in registry["recent_projects"]:
            if not project.get("store_dir"):
                continue
            for state in ACTIVE_RUN_STATES:
                url = f"{project_url(project['id'], self.origin)}api/runs?{urlencode({'status': state})}"
                try:
                    payload = self.fetcher(url, timeout=HTTP_TIMEOUT)
                    runs = payload.get("runs") if isinstance(payload, dict) else None
                    if not isinstance(runs, list) or not all(isinstance(run, dict) for run in runs):
                        fail("HTTP_RESPONSE_INVALID", "Run inventory response is invalid", project_id=project["id"], status=state)
                    for run in runs:
                        run_id = run.get("id")
                        run_status = run.get("status")
                        if not isinstance(run_id, str) or run_status != state:
                            fail("HTTP_RESPONSE_INVALID", "Run inventory contains an inconsistent run", project_id=project["id"], status=state)
                        active.append({"project_id": project["id"], "project_name": project.get("name", ""), "run_id": run_id, "status": state})
                except WorkspaceError as exc:
                    errors.append({"project_id": project["id"], "project_name": project.get("name", ""), "status": state, "code": exc.code, "message": exc.message})
        return {"active_runs": active, "errors": errors}

    def _guard(self, *, force: bool) -> dict[str, Any]:
        check = self._active_work()
        if not force and check["active_runs"]:
            fail("ACTIVE_RUNS", "Shared workspace mutation refused while runs are running or queued", **check)
        if not force and check["errors"]:
            fail("WORKSPACE_CHECK_INCOMPLETE", "Shared workspace mutation refused because the global run check is incomplete", **check)
        return {"forced": force, **check}

    def _wait_ready(self) -> dict[str, Any]:
        deadline = time.monotonic() + READY_TIMEOUT
        last_error: WorkspaceError | None = None
        while time.monotonic() < deadline:
            try:
                ready = self.fetcher(f"{self.origin}/readyz", timeout=HTTP_TIMEOUT)
                if isinstance(ready, dict) and int(ready.get("ready", 0)) > 0 and not ready.get("degraded_project_ids"):
                    return ready
            except WorkspaceError as exc:
                last_error = exc
            self.sleeper(0.25)
        fail(
            "WORKSPACE_NOT_READY",
            f"{SERVICE_NAME} did not become ready within {READY_TIMEOUT} seconds",
            last_error=last_error.message if last_error else "readiness response stayed degraded",
        )

    def mutate(self, action: str, *, force: bool = False) -> dict[str, Any]:
        if action not in {"start", "stop", "restart"}:
            fail("COMMAND_UNKNOWN", f"Unsupported service action: {action}")
        with mutation_lock():
            before = self.service_state()
            guard: dict[str, Any] = {"forced": force, "active_runs": [], "errors": []}
            if action in {"stop", "restart"} and before == "active":
                guard = self._guard(force=force)
                if not force:
                    guard = self._guard(force=False)
            self._systemctl(action)
            readiness = None
            if action in {"start", "restart"}:
                readiness = self._wait_ready()
            after = self.service_state()
        return {
            "scope": "all_registered_projects",
            "service": SERVICE_NAME,
            "action": action,
            "before": before,
            "after": after,
            "guard": guard,
            "readiness": readiness,
        }

    def open_project(self, selector: str | None, *, use_current: bool = False) -> dict[str, Any]:
        context = self.context(selector, use_current=use_current)
        url = context["project"]["url"]
        if not self.opener(url):
            fail("OPEN_FAILED", "The browser could not be opened", url=url)
        return {"opened": True, "url": url, "project": context["project"]}

    def logs(self, *, lines: int = 200, follow: bool = False) -> list[str] | None:
        argv = ["journalctl", "--user", "-u", SERVICE_NAME, "--no-pager", "-n", str(lines)]
        if follow:
            argv.append("--follow")
            try:
                completed = subprocess.run(argv, check=False)
            except OSError as exc:
                fail("COMMAND_FAILED", f"Cannot execute journalctl: {exc}")
            if completed.returncode != 0:
                fail("JOURNAL_FAILED", f"journalctl failed for {SERVICE_NAME}")
            return None
        result = self.runner(argv, timeout=COMMAND_TIMEOUT)
        if result.returncode != 0:
            fail("JOURNAL_FAILED", f"journalctl failed for {SERVICE_NAME}", stderr=result.stderr.strip()[-500:])
        return result.stdout.splitlines()


class CLIParser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        fail("ARGUMENT_INVALID", message)


def parser(prog: str) -> argparse.ArgumentParser:
    root = CLIParser(prog=prog, description="Control the shared Iterion workspace service")
    commands = root.add_subparsers(dest="command", parser_class=CLIParser)

    def add_json(command: argparse.ArgumentParser) -> None:
        command.add_argument("--json", action="store_true")

    for name in ("start", "stop", "restart"):
        command = commands.add_parser(name)
        command.add_argument("names", nargs="*", help=argparse.SUPPRESS)
        if name != "start":
            command.add_argument("--force", action="store_true", help="override active/unknown run protection")
        add_json(command)
    add_json(commands.add_parser("status"))
    add_json(commands.add_parser("list"))
    add_json(commands.add_parser("config"))
    command = commands.add_parser("context")
    command.add_argument("--project")
    command.add_argument("--current", action="store_true")
    command.add_argument("--run", help=argparse.SUPPRESS)
    add_json(command)
    command = commands.add_parser("open")
    command.add_argument("selector", nargs="?")
    command.add_argument("--project")
    command.add_argument("--current", action="store_true")
    add_json(command)
    command = commands.add_parser("logs")
    command.add_argument("selector", nargs="?", help=argparse.SUPPRESS)
    command.add_argument("-n", "--lines", type=int, default=200)
    command.add_argument("-f", "--follow", action="store_true")
    add_json(command)
    return root


def _compat_project_names(controller: Controller, names: Sequence[str]) -> None:
    for name in names:
        if name in {"all", "iterion-workspace", SERVICE_NAME}:
            continue
        resolve_project(controller.registry(), name)


def _with_compat(command: str, value: Any, controller: Controller, selector: str | None = None) -> Any:
    if command == "context" and isinstance(value, dict):
        project = value["project"]
        result = dict(value)
        result["instance"] = {
            "name": "iterion-workspace",
            "state": "up" if value["service"]["state"] == "active" else "down",
            "url": project["url"],
            "project_dir": project["dir"],
            "shared": True,
        }
        return result
    return value


def render_human(command: str, value: Any) -> None:
    if command in {"status", "list"}:
        if command == "status":
            service = value["service"]
            print(f"SERVICE {service['name']}  STATE {service['state']}  ORIGIN {service['origin']}")
        print(f"{'PROJECT':18} {'STATE':10} {'URL':68} DIR")
        for project in value["projects"]:
            print(f"{project['name'][:18]:18} {project['state'][:10]:10} {project['url']:68} {project['dir']}")
    elif command == "context":
        print(value["project"]["url"])
    elif command == "logs":
        print("\n".join(value or []))
    else:
        print(json.dumps(value, indent=2, sort_keys=True))


def emit(command: str, value: Any, *, json_output: bool) -> None:
    if json_output:
        print(json.dumps({"schema_version": SCHEMA_VERSION, "ok": True, "command": command, "result": value}, sort_keys=True))
    else:
        render_human(command, value)


def emit_error(command: str, exc: WorkspaceError, *, json_output: bool, prog: str) -> int:
    payload = {
        "schema_version": SCHEMA_VERSION,
        "ok": False,
        "command": command,
        "error": {"code": exc.code, "message": exc.message, "details": exc.details},
    }
    if json_output:
        print(json.dumps(payload, sort_keys=True))
    else:
        print(f"{prog}: {exc.code}: {exc.message}", file=sys.stderr)
    return 2


def main(argv: Sequence[str] | None = None) -> int:
    args_list = list(sys.argv[1:] if argv is None else argv)
    prog = Path(sys.argv[0]).name
    compat = os.environ.get("ITERION_WORKSPACE_COMPAT") == "1" or prog == "iterion-instances"
    if not args_list:
        args_list = ["status"]
    command_hint = next((item for item in args_list if not item.startswith("-")), "status")
    if command_hint in RETIRED_COMMANDS:
        exc = WorkspaceError(
            "LEGACY_COMMAND_RETIRED",
            f"{command_hint} belonged to per-project deployment management and is retired; use repository build/deployment tooling instead",
        )
        return emit_error(command_hint, exc, json_output="--json" in args_list, prog=prog)
    command = command_hint
    json_output = "--json" in args_list
    try:
        args = parser(prog).parse_args(args_list)
        command = args.command or "status"
        json_output = bool(getattr(args, "json", False))
        controller = Controller()
        if command == "status":
            value = controller.status()
        elif command == "list":
            value = controller.list_projects()
        elif command == "config":
            value = {"registry_file": str(controller.registry_file), "origin": controller.origin, "service": SERVICE_NAME}
        elif command in {"start", "stop", "restart"}:
            names = getattr(args, "names", [])
            if names and not compat:
                fail("PROJECT_ARGUMENT_INVALID", f"{command} controls the shared service and does not accept a project")
            _compat_project_names(controller, names)
            if not json_output:
                print(f"{prog}: {command} affects {SERVICE_NAME} and every registered project", file=sys.stderr)
            value = controller.mutate(command, force=bool(getattr(args, "force", False)))
        elif command == "context":
            if args.run:
                fail("LEGACY_OPTION_RETIRED", "--run context validation belonged to the retired deployment manager")
            value = controller.context(args.project, use_current=args.current)
        elif command == "open":
            selector = args.project or args.selector
            if args.project and args.selector:
                fail("PROJECT_SELECTOR_CONFLICT", "Use either a positional project or --project, not both")
            value = controller.open_project(selector, use_current=args.current)
        elif command == "logs":
            if args.follow and json_output:
                fail("JSON_FOLLOW_UNSUPPORTED", "--json cannot be combined with --follow")
            if args.lines < 0:
                fail("LINES_INVALID", "--lines must be non-negative")
            if args.selector:
                _compat_project_names(controller, [args.selector])
                if not json_output:
                    print(f"{prog}: logs are shared across every registered project", file=sys.stderr)
            value = controller.logs(lines=args.lines, follow=args.follow)
        else:
            fail("COMMAND_UNKNOWN", f"Unknown command: {command}")
        if compat:
            value = _with_compat(command, value, controller, getattr(args, "project", None))
        emit(command, value, json_output=json_output)
        return 0
    except WorkspaceError as exc:
        return emit_error(command, exc, json_output=json_output, prog=prog)


if __name__ == "__main__":
    raise SystemExit(main())
