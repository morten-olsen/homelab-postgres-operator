import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from urllib.error import HTTPError

import release_check as release


SHA = "a" * 40
OTHER = "b" * 40


class ReleaseCheckTests(unittest.TestCase):
    def test_only_canonical_stable_versions(self):
        self.assertEqual(release.version("0.10.2"), (0, 10, 2))
        for value in ["01.2.3", "1.2", "1.2.3-rc.1", "1.2.3+build", "1.2.3\n", "$(id)"]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                release.version(value)

    def test_missing_authoritative_tag_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "absent from Forgejo"):
            release.check_source({}, "v0.0.21", SHA, require_tag=True)

    def test_plan_accepts_an_unused_tag_without_creating_it(self):
        self.assertEqual(release.check_source({}, "v0.0.21", SHA, require_tag=False), [])

    def test_source_tag_must_match_exact_commit_even_in_plan(self):
        for required in [True, False]:
            with self.subTest(required=required), self.assertRaisesRegex(ValueError, "different commit"):
                release.check_source({"refs/tags/v0.0.21": OTHER}, "v0.0.21", SHA, required)

    def test_annotated_tag_uses_peeled_commit(self):
        refs = {"refs/tags/v0.0.21": OTHER, "refs/tags/v0.0.21^{}": SHA,
                "refs/tags/v0.0.20": OTHER}
        self.assertEqual(release.check_source(refs, "v0.0.21", SHA, True), [(0, 0, 20)])

    def test_deleted_tag_cannot_allow_chart_version_reuse(self):
        with self.assertRaisesRegex(ValueError, "published"):
            release.check_version((0, 1, 4), [(0, 0, 15)], [(0, 1, 4)])

    def test_older_version_and_reserved_version_are_rejected(self):
        for candidate in [(0, 0, 19), (0, 0, 20)]:
            with self.subTest(candidate=candidate), self.assertRaisesRegex(ValueError, "reserved"):
                release.check_version(candidate, [(0, 0, 20)], [(0, 0, 18)])

    def test_new_version_compares_numerically(self):
        release.check_version((0, 10, 0), [(0, 9, 9)], [(0, 9, 10)])

    def test_only_http_404_means_artifact_absent(self):
        for code in [401, 403, 429, 500]:
            error = HTTPError("https://example.invalid", code, "unavailable", {}, None)
            with self.subTest(code=code), patch.object(release, "urlopen", side_effect=error):
                with self.assertRaises(HTTPError):
                    release.exists("https://example.invalid")
        error = HTTPError("https://example.invalid", 404, "missing", {}, None)
        with patch.object(release, "urlopen", side_effect=error):
            self.assertFalse(release.exists("https://example.invalid"))

    def test_helm_uses_empty_cache_and_refuses_empty_or_wrong_chart_results(self):
        for results in [[], [{"name": "release-check/other", "version": "0.0.20"}]]:
            with (
                self.subTest(results=results),
                patch.object(release, "command", side_effect=["", json.dumps(results)]),
            ):
                with self.assertRaises(ValueError):
                    release.published_versions("postgres-operator")
        results = [{"name": "release-check/postgres-operator", "version": "0.0.20"}]
        with patch.object(release, "command", side_effect=["", json.dumps(results)]) as command:
            self.assertEqual(release.published_versions("postgres-operator"), [(0, 0, 20)])
            self.assertIn("--repository-cache", command.call_args_list[0].args[0])

    def test_existing_image_or_release_blocks_partial_publication_retry(self):
        for image, metadata in [(True, False), (False, True)]:
            with (
                self.subTest(image=image, metadata=metadata),
                patch.object(release, "image_exists", return_value=image),
                patch.object(release, "exists", return_value=metadata),
            ):
                with self.assertRaisesRegex(ValueError, "already exists"):
                    release.check_artifacts("postgres-operator", "v0.0.21")

    def test_new_image_and_release_are_available(self):
        with patch.object(release, "image_exists", return_value=False), patch.object(release, "exists", return_value=False):
            release.check_artifacts("postgres-operator", "v0.0.21")

    def test_individual_job_retries_cannot_overwrite_published_artifacts(self):
        for stage in ["image", "release"]:
            with (
                self.subTest(stage=stage),
                patch.object(release, "image_exists", return_value=True),
                patch.object(release, "exists", return_value=True),
            ):
                with self.assertRaisesRegex(ValueError, "already exists"):
                    release.check_artifacts("postgres-operator", "v0.0.21", stage)

    def test_release_and_pages_require_the_prior_publication_stage(self):
        for stage, image, metadata in [("release", False, False), ("pages", True, False)]:
            with (
                self.subTest(stage=stage),
                patch.object(release, "image_exists", return_value=image),
                patch.object(release, "exists", return_value=metadata),
            ):
                with self.assertRaisesRegex(ValueError, "missing"):
                    release.check_artifacts("postgres-operator", "v0.0.21", stage)

    def test_normal_stage_handoffs_allow_only_previously_completed_artifacts(self):
        for stage, metadata in [("release", False), ("pages", True)]:
            with (
                self.subTest(stage=stage),
                patch.object(release, "image_exists", return_value=True),
                patch.object(release, "exists", return_value=metadata),
            ):
                release.check_artifacts("postgres-operator", "v0.0.21", stage)

    def test_cli_cannot_publish_a_commit_outside_source_main(self):
        args = ["release_check.py", "--operator", "postgres-operator", "--tag", "v0.0.21", "--commit", SHA]
        failure = subprocess.CalledProcessError(1, ["git", "merge-base"])
        with (
            patch.object(sys, "argv", args),
            patch.object(release, "command", side_effect=[f"{SHA}\trefs/tags/v0.0.21", "", failure]),
            patch.object(release, "check_artifacts") as artifacts,
        ):
            with self.assertRaises(subprocess.CalledProcessError):
                release.main()
            artifacts.assert_not_called()

    def test_cli_outputs_exist_only_after_all_gates_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "outputs"
            args = ["release_check.py", "--operator", "postgres-operator", "--tag", "v0.0.21",
                    "--commit", SHA, "--github-output", str(output)]
            for plan, failure in [(False, True), (True, False), (False, False)]:
                with (
                    self.subTest(plan=plan, failure=failure),
                    patch.object(sys, "argv", args + (["--plan"] if plan else [])),
                    patch.object(release, "command", side_effect=[f"{SHA}\trefs/tags/v0.0.21", "", ""]),
                    patch.object(release, "published_versions", return_value=[(0, 0, 20)]),
                    patch.object(release, "check_artifacts", side_effect=ValueError("exists") if failure else None),
                    patch("builtins.print"),
                ):
                    if plan or failure:
                        with self.assertRaises(ValueError):
                            release.main()
                        self.assertFalse(output.exists())
                    else:
                        release.main()
                        self.assertEqual(output.read_text(), "new_tag=v0.0.21\nnew_version=0.0.21\n")


class MirrorRegressionTests(unittest.TestCase):
    def test_downstream_only_tag_is_deleted_but_source_tag_survives(self):
        # Reproduce the production mirror mechanism entirely in temporary local repositories.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, downstream = root / "source", root / "downstream.git"
            def git(*args):
                return subprocess.run(["git", *map(str, args)], check=True,
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True).stdout.strip()
            git("init", source)
            git("-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                "commit", "--allow-empty", "-m", "fixture")
            git("init", "--bare", downstream)
            git("-C", source, "push", "--mirror", downstream)
            commit = git("-C", source, "rev-parse", "HEAD")
            git("--git-dir", downstream, "update-ref", "refs/tags/v0.0.19", commit)
            git("-C", source, "push", "--mirror", downstream)
            self.assertEqual(git("--git-dir", downstream, "tag", "-l"), "")
            git("-C", source, "tag", "v0.0.19")
            git("-C", source, "push", "--mirror", downstream)
            git("-C", source, "push", "--mirror", downstream)
            self.assertEqual(git("--git-dir", downstream, "rev-parse", "refs/tags/v0.0.19"), commit)


if __name__ == "__main__":
    unittest.main()
