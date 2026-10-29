import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import release_check as release

OPERATOR = 'postgres-operator'
TAG = 'v0.0.22'
SHA = 'a' * 40
MIRROR = 'https://github.com/morten-olsen/homelab-' + OPERATOR + '.git'
SOURCE = 'https://code.olsen.cloud/homelab/' + OPERATOR + '.git'


class SourceTests(unittest.TestCase):
    def test_plans_read_forgejo_but_publishers_read_only_the_outbound_mirror(self):
        for plan, expected in [(True, SOURCE), (False, MIRROR)]:
            with self.subTest(plan=plan), patch.object(release, 'command', side_effect=[SHA + '\trefs/heads/main', '', SHA]) as command:
                self.assertEqual(release.source_snapshot(OPERATOR, plan), {'refs/heads/main': SHA})
                self.assertIn(expected, command.call_args_list[0].args[0])
                self.assertIn(expected, command.call_args_list[1].args[0])

    def test_missing_main_and_source_races_fail_closed(self):
        for output, fetched in [('', SHA), ('bad\trefs/heads/main', SHA), (SHA + '\trefs/heads/main', 'b' * 40)]:
            with self.subTest(output=output, fetched=fetched), patch.object(release, 'command', side_effect=[output, '', fetched]):
                with self.assertRaisesRegex(ValueError, 'main'):
                    release.source_snapshot(OPERATOR)

    def test_inspection_cannot_supply_publication_inputs_or_outputs(self):
        for extra in [['--tag', TAG], ['--commit', SHA], ['--github-output', '/tmp/forbidden'], ['--plan'], ['--stage', 'pages']]:
            with self.subTest(extra=extra), patch.object(sys, 'argv', ['release_check.py', '--operator', OPERATOR, '--check-source', *extra]):
                with self.assertRaisesRegex(ValueError, 'inspection'):
                    release.main()

    def test_inspection_probes_mirror_without_looking_up_or_publishing_artifacts(self):
        with patch.object(sys, 'argv', ['release_check.py', '--operator', OPERATOR, '--check-source']), \
             patch.object(release, 'source_snapshot', return_value={'refs/heads/main': SHA}) as source, \
             patch.object(release, 'check_artifacts') as artifacts, patch('builtins.print'):
            release.main()
            source.assert_called_once_with(OPERATOR)
            artifacts.assert_not_called()


class MirrorSourceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.repo = Path(self.directory.name) / 'source'
        self.git('init', '-b', 'main', self.repo)
        self.git('config', 'user.name', 'Release test', cwd=self.repo)
        self.git('config', 'user.email', 'release@example.invalid', cwd=self.repo)
        self.git('commit', '--allow-empty', '-m', 'approved source', cwd=self.repo)
        self.commit = self.git('rev-parse', 'HEAD', cwd=self.repo)
        self.git('tag', TAG, self.commit, cwd=self.repo)
        self.downstream = Path(self.directory.name) / 'mirror.git'
        self.git('init', '--bare', self.downstream)
        self.sync()
        real_command = release.command
        def command(args):
            self.assertFalse(any('code.olsen.cloud' in a for a in args), args)
            return real_command([str(self.downstream) if a == MIRROR else a for a in args])
        self.patcher = patch.object(release, 'command', side_effect=command)
        self.patcher.start()
        self.addCleanup(self.patcher.stop)

    def git(self, *args, cwd=None):
        return subprocess.check_output(['git', *map(str, args)], cwd=cwd, text=True, stderr=subprocess.DEVNULL).strip()

    def sync(self):
        self.git('push', '--mirror', self.downstream, cwd=self.repo)

    def verify(self, commit=None):
        before = os.getcwd()
        try:
            os.chdir(self.repo)
            return release.verify_source(OPERATOR, TAG, commit or self.commit)
        finally:
            os.chdir(before)

    def test_real_tag_pipeline_reads_mirror_and_exports_only_the_exact_approved_commit(self):
        self.assertEqual(self.verify(), [])
        output = Path(self.directory.name) / 'outputs'
        before = os.getcwd()
        try:
            os.chdir(self.repo)
            with patch.object(sys, 'argv', ['release_check.py', '--operator', OPERATOR, '--tag', TAG,
                                          '--commit', self.commit, '--github-output', str(output)]), \
                 patch.object(release, 'published_versions', return_value=[(0, 0, 20)]), \
                 patch.object(release, 'check_artifacts'):
                release.main()
            self.assertEqual(output.read_text(), f'new_tag={TAG}\nnew_version={TAG[1:]}\nrelease_commit={self.commit}\n')
        finally:
            os.chdir(before)

    def test_missing_mirrored_tag_is_rejected(self):
        self.git('tag', '-d', TAG, cwd=self.repo)
        self.sync()
        with self.assertRaisesRegex(ValueError, 'absent'):
            self.verify()

    def test_wrong_tag_commit_is_rejected(self):
        self.git('commit', '--allow-empty', '-m', 'later', cwd=self.repo)
        with self.assertRaisesRegex(ValueError, 'different commit'):
            self.verify(self.git('rev-parse', 'HEAD', cwd=self.repo))

    def test_matching_tag_on_an_unmerged_branch_cannot_publish(self):
        self.git('switch', '-c', 'unmerged', cwd=self.repo)
        self.git('commit', '--allow-empty', '-m', 'unmerged', cwd=self.repo)
        foreign = self.git('rev-parse', 'HEAD', cwd=self.repo)
        self.git('tag', '-f', TAG, foreign, cwd=self.repo)
        self.git('switch', 'main', cwd=self.repo)
        self.sync()
        with self.assertRaises(subprocess.CalledProcessError):
            self.verify(foreign)

    def test_unused_artifact_cannot_reuse_an_older_reserved_version(self):
        self.git('tag', 'v0.0.23', self.commit, cwd=self.repo)
        self.sync()
        reserved = self.verify()
        self.assertIn((0, 0, 23), reserved)
        with self.assertRaisesRegex(ValueError, 'reserved'):
            release.check_version((0, 0, 22), reserved, [(0, 0, 20)])


if __name__ == '__main__':
    unittest.main()
