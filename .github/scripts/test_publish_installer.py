import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

import publish_installer as publish


class InstallerPackageTests(unittest.TestCase):
    def settings(self):
        return {
            'GITHUB_REPOSITORY': 'dl10211021/prometheus-lifecycle-platform',
            'GITHUB_REF': 'refs/heads/main',
            'RELEASE_TAG': 'v1.0.0',
            'ASSET_NAME': 'promlifecycle-20261012-linux-amd64-offline-public.tar.gz',
            'EXPECTED_SHA256': 'a' * 64,
        }

    def test_inputs_are_fixed_to_release_files_and_main_repository(self):
        self.assertEqual(publish.validate_inputs(self.settings()), ('v1.0.0', self.settings()['ASSET_NAME'], 'a' * 64))
        invalid = {
            'GITHUB_REPOSITORY': ['another/owner'],
            'GITHUB_REF': ['refs/pull/1/merge', 'refs/heads/unreviewed'],
            'RELEASE_TAG': ['latest', 'v01.0.0', 'v1.0.0;id', 'v1.0.0\n'],
            'ASSET_NAME': ['../../secret.tar.gz', '--option.tar.gz', 'promlifecycle-$(id).tar.gz', 'promlifecycle-x.zip'],
            'EXPECTED_SHA256': ['a' * 63, 'G' * 64, 'a' * 64 + '\n'],
        }
        for field, values in invalid.items():
            for value in values:
                with self.subTest(field=field, value=value):
                    env = self.settings()
                    env[field] = value
                    with self.assertRaises(ValueError):
                        publish.validate_inputs(env)

    def test_only_explicit_registry_404_permits_new_tag(self):
        class Registry:
            def __init__(self, status):
                self.status = status
                self.requests = []

            def open(self, request, timeout):
                self.requests.append(request)
                if len(self.requests) == 1:
                    return io.BytesIO(b'{"token":"registry-test-token"}')
                if self.status != 200:
                    raise HTTPError(request.full_url, self.status, 'fixture', {}, None)
                return io.BytesIO(b'')

        for status in [200, 404, 401, 403, 429, 500]:
            with self.subTest(status=status):
                registry = Registry(status)
                with patch.object(publish.urllib.request, 'build_opener', return_value=registry):
                    if status == 404:
                        publish.ensure_tag_absent('v1.0.0', 'fixture-user', 'fixture-secret')
                    else:
                        with self.assertRaises(RuntimeError):
                            publish.ensure_tag_absent('v1.0.0', 'fixture-user', 'fixture-secret')
                self.assertEqual(registry.requests[-1].method, 'HEAD')
                self.assertNotIn('fixture-secret', registry.requests[0].full_url)
                self.assertEqual(registry.requests[-1].get_header('Authorization'), 'Bearer registry-test-token')

    def test_wrong_release_digest_stops_before_registry_authentication(self):
        env = self.settings()
        env.update(GITHUB_ACTOR='fixture-user', GITHUB_TOKEN='fixture-secret')
        with patch.dict(publish.os.environ, env, clear=True), patch.object(publish.urllib.request, 'urlopen', return_value=io.BytesIO(b'tampered release')), patch.object(publish.subprocess, 'run') as run, patch.object(publish, 'ensure_tag_absent') as preflight:
            with self.assertRaisesRegex(RuntimeError, 'SHA256 mismatch'):
                publish.main()
            run.assert_not_called()
            preflight.assert_not_called()

    def test_actual_oci_artifact_and_pull_roundtrip(self):
        oras = os.environ.get('ORAS_TEST_BIN', 'oras')
        with tempfile.TemporaryDirectory(prefix='plc-oci-test-') as directory:
            root = Path(directory)
            asset = 'promlifecycle-format-test.tar.gz'
            with tarfile.open(root / asset, 'w:gz') as archive:
                data = b'#!/bin/sh\nprintf "native systemd installer fixture\\n"\n'
                member = tarfile.TarInfo('fixture/install.sh')
                member.mode = 0o755
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
            expected = hashlib.sha256((root / asset).read_bytes()).hexdigest()
            checksum = expected + '  ' + asset + '\n'
            (root / (asset + '.sha256')).write_text(checksum)
            ref = str(root / 'layout') + ':v1.0.0'
            manifest = root / 'manifest.json'
            args = publish.push_args(ref, asset, manifest, 'v1.0.0', expected)
            subprocess.run([oras, *args, '--oci-layout'], cwd=root, check=True, capture_output=True)
            payload = json.loads(manifest.read_text())
            self.assertEqual(payload['artifactType'], 'application/vnd.promlifecycle.installer.v1')
            self.assertEqual(payload['annotations']['org.opencontainers.image.source'], 'https://github.com/dl10211021/prometheus-lifecycle-platform')
            self.assertEqual(payload['annotations']['org.opencontainers.image.version'], 'v1.0.0')
            self.assertEqual(len(payload['layers']), 2)
            self.assertEqual(payload['layers'][0]['mediaType'], 'application/gzip')
            self.assertEqual(payload['layers'][0]['digest'], 'sha256:' + expected)
            self.assertEqual(payload['layers'][0]['annotations']['org.opencontainers.image.title'], asset)
            self.assertEqual(payload['layers'][1]['mediaType'], 'text/plain')
            self.assertEqual(payload['layers'][1]['annotations']['org.opencontainers.image.title'], asset + '.sha256')
            digest = 'sha256:' + hashlib.sha256(manifest.read_bytes()).hexdigest()
            resolved = subprocess.check_output([oras, 'resolve', '--oci-layout', ref], text=True).strip()
            self.assertEqual(resolved, digest)
            subprocess.run([oras, 'pull', '--oci-layout', str(root / 'layout') + '@' + digest, '--output', str(root / 'pulled')], check=True, capture_output=True)
            self.assertEqual(hashlib.sha256((root / 'pulled' / asset).read_bytes()).hexdigest(), expected)
            self.assertEqual((root / 'pulled' / (asset + '.sha256')).read_text(), checksum)
            subprocess.run(['sha256sum', '--check', asset + '.sha256'], cwd=root / 'pulled', check=True, capture_output=True)


if __name__ == '__main__':
    unittest.main()
