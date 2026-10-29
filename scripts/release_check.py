#!/usr/bin/env python3
"""Read-only release preflight; tags originate in Forgejo and publishers read its outward mirror."""

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
        raise ValueError(f"{tag} is absent from the release source")
    if actual is not None and actual != commit:
        raise ValueError(f"{tag} points to a different commit in the release source")
    reserved = []
    for name in refs:
        if name.startswith("refs/tags/v") and not name.endswith("^{}") and name != ref:
            value = name.removeprefix("refs/tags/v")
            if SEMVER.fullmatch(value):
                reserved.append(version(value))
    return reserved


def source_snapshot(operator, plan=False):
    # GitHub receives these refs through the existing authenticated Forgejo push mirror.
    # Hosted runners never connect to the private home network. Local plans read Forgejo.
    source = (f"https://code.olsen.cloud/homelab/{operator}.git" if plan else
              f"https://github.com/morten-olsen/homelab-{operator}.git")
    output = command(["git", "ls-remote", source, "refs/heads/main", "refs/tags/v*"])
    refs = dict(line.split()[::-1] for line in output.splitlines())
    main = refs.get("refs/heads/main", "")
    if not re.fullmatch(r"[0-9a-f]{40}", main):
        raise ValueError("Release source main is missing or invalid")
    command(["git", "fetch", "--no-tags", source, "refs/heads/main"])
    if command(["git", "rev-parse", "FETCH_HEAD"]) != main:
        raise ValueError("Release source main changed during verification; retry the read-only check")
    return refs


def verify_source(operator, tag, commit, plan=False):
    refs = source_snapshot(operator, plan)
    reserved = check_source(refs, tag, commit, require_tag=not plan)
    command(["git", "merge-base", "--is-ancestor", commit, refs["refs/heads/main"]])
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


def workflow_release(event, operator):
    """Treat workflow_run metadata as untrusted; source checks still follow."""
    repository = f"morten-olsen/homelab-{operator}"
    run = event["workflow_run"]
    if (event["repository"]["full_name"] != repository
            or run["head_repository"]["full_name"] != repository
            or run["event"] != "push" or run["conclusion"] != "success"
            or run["path"] != ".github/workflows/release.yml" or run["name"] != "Release"):
        raise ValueError("Not a successful release push in this repository")
    tag, commit = run["head_branch"], run["head_sha"]
    if not tag.startswith("v") or not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("Invalid release tag or commit in workflow handoff")
    version(tag[1:])
    return tag, commit


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--operator", choices=OPERATORS, required=True)
    parser.add_argument("--tag")
    parser.add_argument("--check-source", action="store_true",
                        help="Inspect read-only outbound mirror access; never publish")
    parser.add_argument("--commit", help="Full source commit SHA")
    parser.add_argument("--workflow-run", type=Path, help="Validate a completed workflow_run event file")
    parser.add_argument("--plan", action="store_true", help="Allow an absent source tag; never create it")
    parser.add_argument("--stage", choices=("image", "release", "pages"), default="image")
    parser.add_argument("--github-output", type=Path)
    args = parser.parse_args()
    if args.check_source:
        if args.tag or args.commit or args.workflow_run or args.plan or args.github_output or args.stage != "image":
            raise ValueError("Source inspection cannot supply publication inputs or outputs")
        refs = source_snapshot(args.operator)
        print(f"Source inspection passed: {args.operator}; mirrored main={refs['refs/heads/main']}")
        return
    if args.workflow_run:
        if args.tag or args.commit or args.plan or args.stage != "pages":
            raise ValueError("Workflow handoff is only for the Pages stage")
        args.tag, args.commit = workflow_release(json.loads(args.workflow_run.read_text()), args.operator)
    if not args.tag or not args.commit:
        raise ValueError("Both tag and commit are required")
    if args.plan and (args.github_output or args.stage != "image"):
        raise ValueError("Plan mode cannot supply publication outputs or resume a publication stage")
    if not args.tag.startswith("v"):
        raise ValueError("Release tag must start with v")
    candidate = version(args.tag[1:])
    if not re.fullmatch(r"[0-9a-f]{40}", args.commit):
        raise ValueError("Commit must be a full lowercase SHA")
    reserved = verify_source(args.operator, args.tag, args.commit, args.plan)
    charts = published_versions(args.operator)
    check_version(candidate, reserved, charts)
    check_artifacts(args.operator, args.tag, args.stage)
    print(f"Preflight passed: {args.operator} {args.tag} at {args.commit}; plan={args.plan}")
    if args.github_output:
        with args.github_output.open("a") as output:
            output.write(f"new_tag={args.tag}\nnew_version={args.tag[1:]}\nrelease_commit={args.commit}\n")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print(f"Release refused: {error}", file=sys.stderr)
        sys.exit(1)
