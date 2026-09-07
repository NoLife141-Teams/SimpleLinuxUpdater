#!/usr/bin/env python3
"""Trusted policy executed from the default-branch checkout, never the release tag."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def version(tag):
    match = re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", tag)
    if not match:
        raise ValueError(f"Invalid stable release tag: {tag}")
    return tuple(map(int, match.groups()))


def releases(repo):
    # API failures are fatal; a failed request is never interpreted as an absent release.
    pages = json.loads(command("gh", "api", "--paginate", "--slurp", f"repos/{repo}/releases?per_page=100"))
    return [release for page in pages for release in page]


def may_promote(tag, records, tags):
    current = version(tag)
    # Drafts reserve their version as well: a failed newer attempt must not let an
    # older retry regress latest. Reachable main tags also cover queued runs.
    candidates = tags + [r["tag_name"] for r in records]
    return not any(version(t) > current for t in candidates if re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", t))


def main():
    mode = sys.argv[1]
    tag, repo = os.environ["RELEASE_TAG"], os.environ["GITHUB_REPOSITORY"]
    version(tag)
    records = releases(repo)
    existing = next((r for r in records if r["tag_name"] == tag), None)
    if existing and not existing["draft"]:
        raise RuntimeError("Refusing to replace an already published release")
    if mode == "draft":
        return
    if mode != "finalize" or existing is None:
        raise RuntimeError("Finalization requires an existing draft release")
    command("git", "fetch", "--force", "origin", "refs/heads/main:refs/remotes/origin/main", "+refs/tags/*:refs/tags/*")
    tags = command("git", "tag", "--merged", "origin/main", "--list", "v*").splitlines()
    # Recheck identity after the potentially long build and runtime tests.
    command("bash", str(Path(__file__).with_name("verify-tag-on-main.sh")))
    promote = may_promote(tag, records, tags)
    if promote:
        digest = os.environ["DIGEST"]
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            raise ValueError("Invalid verified image digest")
        command("docker", "buildx", "imagetools", "create", "--tag", os.environ["IMAGE"] + ":latest", os.environ["IMAGE"] + "@" + digest)
    command("gh", "release", "edit", tag, "--repo", repo, "--draft=false", "--latest=" + str(promote).lower())
    print(f"Published {tag}; promoted latest={promote}")


if __name__ == "__main__":
    main()
