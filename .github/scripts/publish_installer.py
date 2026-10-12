#!/usr/bin/env python3
"""Publish a verified native installer archive as an OCI file artifact."""
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request

REPOSITORY = 'dl10211021/prometheus-lifecycle-platform'
PACKAGE = 'ghcr.io/' + REPOSITORY + '-installer'
SOURCE = 'https://github.com/' + REPOSITORY


def validate_inputs(env):
    if env.get('GITHUB_REPOSITORY') != REPOSITORY or env.get('GITHUB_REF') != 'refs/heads/main':
        raise ValueError('Publication is restricted to the source repository main branch')
    tag, asset, digest = (env.get(key, '') for key in ('RELEASE_TAG', 'ASSET_NAME', 'EXPECTED_SHA256'))
    if len(tag) > 64 or not re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', tag):
        raise ValueError('release_tag must be a stable version such as v1.0.0')
    if len(asset) > 180 or not re.fullmatch(r'promlifecycle-[A-Za-z0-9][A-Za-z0-9._-]*\.tar\.gz', asset):
        raise ValueError('asset_name must be a promlifecycle-*.tar.gz basename')
    if not re.fullmatch(r'[0-9a-f]{64}', digest):
        raise ValueError('expected_sha256 must contain exactly 64 lowercase hex characters')
    return tag, asset, digest


class NoAuthRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def ensure_tag_absent(tag, actor, token):
    """Only a definite registry 404 permits publishing; auth/errors fail closed."""
    scope = 'repository:' + REPOSITORY + '-installer:pull,push'
    url = 'https://ghcr.io/token?' + urllib.parse.urlencode({'service': 'ghcr.io', 'scope': scope})
    basic = base64.b64encode((actor + ':' + token).encode()).decode()
    opener = urllib.request.build_opener(NoAuthRedirect())
    request = urllib.request.Request(url, headers={'Authorization': 'Basic ' + basic})
    try:
        with opener.open(request, timeout=30) as response:
            body = json.load(response)
    except urllib.error.HTTPError as error:
        code = error.code
        error.close()
        raise RuntimeError('Registry authorization failed: HTTP ' + str(code)) from None
    bearer = body.get('token') or body.get('access_token')
    if not isinstance(bearer, str) or not bearer:
        raise RuntimeError('Registry returned no access token')
    request = urllib.request.Request(
        'https://ghcr.io/v2/' + REPOSITORY + '-installer/manifests/' + tag,
        headers={'Authorization': 'Bearer ' + bearer, 'Accept': 'application/vnd.oci.image.manifest.v1+json'},
        method='HEAD',
    )
    try:
        with opener.open(request, timeout=30):
            pass
    except urllib.error.HTTPError as error:
        code = error.code
        error.close()
        if code == 404:
            return
        raise RuntimeError('Registry tag preflight failed: HTTP ' + str(code)) from None
    raise RuntimeError('Version tag already exists; refusing to overwrite ' + tag)


def push_args(reference, asset, manifest, tag, digest):
    return [
        'push', '--artifact-type', 'application/vnd.promlifecycle.installer.v1',
        '--annotation', 'org.opencontainers.image.source=' + SOURCE,
        '--annotation', 'org.opencontainers.image.version=' + tag,
        '--annotation', 'org.opencontainers.image.description=Native Linux amd64 offline installer; not a runnable container image',
        '--annotation', 'io.promlifecycle.archive.sha256=' + digest,
        '--export-manifest', str(manifest), reference, asset + ':application/gzip',
        asset + '.sha256:text/plain',
    ]


def sha256(path):
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(block)
    return digest.hexdigest()


def main():
    tag, asset, expected = validate_inputs(os.environ)
    actor, token = os.environ.get('GITHUB_ACTOR', ''), os.environ.get('GITHUB_TOKEN', '')
    if not actor or not token:
        raise ValueError('GitHub Actions actor and job token are required')
    os.umask(0o077)
    reference = PACKAGE + ':' + tag
    with tempfile.TemporaryDirectory(prefix='plc-ghcr-') as directory:
        root = Path(directory)
        archive = root / asset
        # This request carries no credentials: a draft/private Release cannot be downloaded.
        url = SOURCE + '/releases/download/' + tag + '/' + asset
        with urllib.request.urlopen(url, timeout=60) as response, archive.open('wb') as output:
            shutil.copyfileobj(response, output, 1024 * 1024)
        if sha256(archive) != expected:
            raise RuntimeError('Release asset SHA256 mismatch; no registry mutation attempted')
        checksum = expected + '  ' + asset + '\n'
        (root / (asset + '.sha256')).write_text(checksum)
        auth = root / 'registry-auth.json'
        subprocess.run(['oras', 'login', 'ghcr.io', '--username', actor, '--password-stdin', '--registry-config', str(auth)], input=token + '\n', text=True, check=True)
        ensure_tag_absent(tag, actor, token)
        manifest = root / 'manifest.json'
        subprocess.run(['oras', *push_args(reference, asset, manifest, tag, expected), '--registry-config', str(auth)], cwd=root, check=True)
        digest = 'sha256:' + sha256(manifest)
        resolved = subprocess.check_output(['oras', 'resolve', reference, '--registry-config', str(auth)], text=True).strip()
        if resolved != digest:
            raise RuntimeError('Published tag does not resolve to the manifest just uploaded')
        immutable = PACKAGE + '@' + digest
        pulled = root / 'pulled'
        subprocess.run(['oras', 'pull', immutable, '--output', str(pulled), '--registry-config', str(auth)], check=True)
        if sha256(pulled / asset) != expected:
            raise RuntimeError('Registry pull-back SHA256 mismatch')
        if (pulled / (asset + '.sha256')).read_text() != checksum:
            raise RuntimeError('Registry checksum-file pull-back mismatch')
        result = {'package': reference, 'immutable_reference': immutable, 'asset': asset, 'archive_sha256': expected, 'pull_back_verified': True}
        print(json.dumps(result, indent=2))
        if os.environ.get('GITHUB_STEP_SUMMARY'):
            with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as summary:
                summary.write('Verified native installer OCI artifact (not a runnable container image):\n\n```json\n' + json.dumps(result, indent=2) + '\n```\n\nNew GHCR packages default to private. Set package visibility to Public separately, then verify anonymous pull.\n')


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print('Publication stopped: ' + str(error), file=sys.stderr)
        sys.exit(1)
