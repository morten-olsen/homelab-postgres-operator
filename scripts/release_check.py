#!/usr/bin/env python3
"""Read-only release preflight. Tags are created only in Forgejo, after approval."""

import argparse
import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen


OPERATORS = ("postgres-operator", "authentik-operator", "dns-operator", "nuclei-operator")
SEMVER = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")


def version(value):
    if not SEMVER.fullmatch(value):
        raise ValueError(f"Not a canonical stable version: {value!r}")
    return tuple(map(int, value.split(".")))


def command(args):
    return subprocess.check_output(args, text=True, timeout=120).strip()


def check_source(refs, tag, commit, require_tag):
    ref = f"refs/tags/{tag}"
    actual = refs.get(ref + "^{}", refs.get(ref))
    if actual is None and require_tag:
        raise ValueError(f"{tag} is absent from Forgejo")
    if actual is not None and actual != commit:
        raise ValueError(f"{tag} points to a different commit in Forgejo")
    reserved = []
    for name in refs:
        if name.startswith("refs/tags/v") and not name.endswith("^{}") and name != ref:
            value = name.removeprefix("refs/tags/v")
            if SEMVER.fullmatch(value):
                reserved.append(version(value))
    return reserved


def check_version(candidate, reserved, published):
    if reserved and candidate <= max(reserved):
        raise ValueError("Version must be newer than all other Forgejo reserved tags")
    if published and candidate <= max(published):
        raise ValueError("Version must be newer than all published charts, even if their tags are missing")


def published_versions(operator):
    # Helm parses its own YAML format. A fresh cache prevents an outage from using stale results.
    with tempfile.TemporaryDirectory() as directory:
        options = ["--repository-cache", directory, "--repository-config", f"{directory}/repos.yaml"]
        command(["helm", "repo", "add", "release-check",
                 f"https://morten-olsen.github.io/homelab-{operator}", *options])
        charts = json.loads(command(["helm", "search", "repo", f"release-check/{operator}",
                                    "--versions", "--output", "json", *options]))
    if not charts or any(chart["name"] != f"release-check/{operator}" for chart in charts):
        raise ValueError("Published chart history is missing or unexpected; investigate before releasing")
    return [version(chart["version"]) for chart in charts]


def exists(url, headers=None):
    try:
        with urlopen(Request(url, headers=headers or {}), timeout=30):
            return True
    except HTTPError as error:
        if error.code == 404:
            return False
        raise


def image_exists(operator, value):
    # Anonymous pull authorization for the existing public package; never log its bearer token.
    repository = f"morten-olsen/homelab-{operator}"
    with urlopen(f"https://ghcr.io/token?service=ghcr.io&scope=repository:{repository}:pull", timeout=30) as response:
        token = json.load(response)["token"]
    return exists(f"https://ghcr.io/v2/{repository}/manifests/{value}", {
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json",
    })


def check_artifacts(operator, tag, stage="image"):
    image = image_exists(operator, tag[1:])
    if stage == "image" and image:
        raise ValueError("Image version already exists; partial publication requires an explicit recovery decision")
    if stage != "image" and not image:
        raise ValueError("Prior image publication is missing")
    metadata = exists(f"https://api.github.com/repos/morten-olsen/homelab-{operator}/releases/tags/{tag}")
    if stage != "pages" and metadata:
        raise ValueError("Release already exists; do not overwrite it")
    if stage == "pages" and not metadata:
        raise ValueError("Prior release publication is missing")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--operator", choices=OPERATORS, required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--commit", required=True, help="Full source commit SHA")
    parser.add_argument("--plan", action="store_true", help="Allow an absent source tag; never create it")
    parser.add_argument("--stage", choices=("image", "release", "pages"), default="image")
    parser.add_argument("--github-output", type=Path)
    args = parser.parse_args()
    if args.plan and (args.github_output or args.stage != "image"):
        raise ValueError("Plan mode cannot supply publication outputs or resume a publication stage")
    if not args.tag.startswith("v"):
        raise ValueError("Release tag must start with v")
    candidate = version(args.tag[1:])
    if not re.fullmatch(r"[0-9a-f]{40}", args.commit):
        raise ValueError("Commit must be a full lowercase SHA")
    source = f"https://code.olsen.cloud/homelab/{args.operator}.git"
    output = command(["git", "ls-remote", source, "refs/heads/main", "refs/tags/v*"])
    refs = dict(line.split()[::-1] for line in output.splitlines())
    reserved = check_source(refs, args.tag, args.commit, require_tag=not args.plan)
    # Fetch only authoritative main, not mirrored tags. The approved commit must belong to main.
    command(["git", "fetch", "--no-tags", source, "refs/heads/main"])
    command(["git", "merge-base", "--is-ancestor", args.commit, "FETCH_HEAD"])
    charts = published_versions(args.operator)
    check_version(candidate, reserved, charts)
    check_artifacts(args.operator, args.tag, args.stage)
    print(f"Preflight passed: {args.operator} {args.tag} at {args.commit}; plan={args.plan}")
    if args.github_output:
        with args.github_output.open("a") as output:
            output.write(f"new_tag={args.tag}\nnew_version={args.tag[1:]}\n")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print(f"Release refused: {error}", file=sys.stderr)
        sys.exit(1)
