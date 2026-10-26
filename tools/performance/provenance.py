"""Record source identity without inspecting untracked prototypes or app data."""
import hashlib
import json
import os
import platform
import subprocess
import sys
from pathlib import Path


def command(*args):
    return subprocess.check_output(args, text=True).strip()


# Optional source-root/ref permit hashing an exact Git archive while keeping
# runtime/tool discovery in the current, dependency-equipped checkout.
source_root = Path(sys.argv[1]) if len(sys.argv) > 1 else Path.cwd()
ref = sys.argv[2] if len(sys.argv) > 2 else "HEAD"
paths = subprocess.check_output(["git", "ls-tree", "-r", "-z", "--name-only", ref]).decode().split("\0")
paths = [name for name in paths if name.endswith(".go") or name in ("go.mod", "go.sum") or name.startswith(("templates/", "static/js/", "static/css/"))]
digest = hashlib.sha256()
for name in sorted(p for p in paths if p and not p.endswith("_test.go") and "/testutil/" not in p):
    digest.update(name.encode() + b"\0" + (source_root / name).read_bytes() + b"\0")
print(json.dumps({
    "head": command("git", "rev-parse", ref),
    "source_root": str(source_root),
    "production_source_sha256": digest.hexdigest(),
    "hash_recipe": "sha256 of sorted tracked production Go, go.mod, go.sum, templates, static/js, static/css: filename NUL contents NUL; excludes _test.go and /testutil/",
    "os": platform.platform(),
    "cpu": next(line.split(":", 1)[1].strip() for line in Path("/proc/cpuinfo").read_text().splitlines() if line.startswith("model name")),
    "logical_cpus": os.cpu_count(),
    "meminfo": [line for line in Path("/proc/meminfo").read_text().splitlines() if line.startswith(("MemTotal:", "MemAvailable:"))],
    "go": command("go", "version"),
    "node": command("node", "--version"),
    "playwright": command("node", "-p", 'require("@playwright/test/package.json").version'),
    "gomaxprocs": os.environ.get("GOMAXPROCS", "Go default"),
    "utc": command("date", "-u", "+%Y-%m-%dT%H:%M:%SZ"),
}, indent=2))
