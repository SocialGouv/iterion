#!/usr/bin/env python3
"""Install the local instance manager from one committed source checkout."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile


def git(*args: str, cwd: Path) -> str:
    return subprocess.check_output(["git", "-C", str(cwd), *args], text=True).strip()


def atomic_copy(source: Path, target: Path, mode: int) -> None:
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{target.name}.", dir=target.parent)
    os.close(fd)
    try:
        shutil.copyfile(source, temporary)
        os.chmod(temporary, mode)
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--prefix", default=str(Path.home() / ".local"))
    parser.add_argument("--allow-dirty", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()

    source_dir = Path(__file__).resolve().parent
    repo = Path(git("rev-parse", "--show-toplevel", cwd=source_dir))
    commit = git("rev-parse", "HEAD^{commit}", cwd=repo)
    dirty = git("status", "--porcelain", "--untracked-files=all", cwd=repo)
    if dirty and not args.allow_dirty:
        raise SystemExit("refus d'installer depuis un worktree non committé")

    prefix = Path(args.prefix).expanduser().resolve()
    backend = prefix / "share/iterion-instance-manager/instances.py"
    wrapper = prefix / "bin/iterion-instances"
    receipt = prefix / "share/iterion-instance-manager/install.json"
    atomic_copy(source_dir / "instances.py", backend, 0o644)
    atomic_copy(source_dir / "iterion-instances", wrapper, 0o755)
    metadata = {
        "schema_version": 1,
        "source_repository": str(repo),
        "source_commit": commit,
        "backend_sha256": hashlib.sha256(backend.read_bytes()).hexdigest(),
    }
    receipt.parent.mkdir(parents=True, exist_ok=True)
    receipt.write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n")
    os.chmod(receipt, stat.S_IRUSR | stat.S_IWUSR)
    print(json.dumps({"installed": True, **metadata, "command": str(wrapper)}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
