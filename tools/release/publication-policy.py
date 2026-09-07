#!/usr/bin/env python3
"""Trusted, resumable publication of the exact qualified candidate digest."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

from publication_registry import image_for_repo, registry_digest, require_digest

PLATFORMS = ['linux/amd64', 'linux/arm64']
CHECKS = ['runtime-persistence', 'trivy-os-and-go']
ASSET = 'publication.json'


def capture_command(*args):
    return subprocess.check_output(list(args), text=True).strip()


def run_command(*args):
    # Inherit stdout/stderr so validation output is visible while the child runs.
    subprocess.run(list(args), check=True)


def validation_results():
    return [dict(platform=platform, check=check, script=script, result='not executed')
            for platform in PLATFORMS
            for check, script in [('startup/persistence', 'docker-smoke.sh'),
                                  ('OS/Go scan', 'scan-image.sh')]]


def write_validation_summary(expected, digest, results, *, note='Publication finalization is a separate step; these are validation results only.'):
    path = os.environ.get('GITHUB_STEP_SUMMARY')
    if not path:
        return
    lines = ['### Release validations', '', f"Version: `{expected['tag']}`",
             f'Digest: `{digest}`', '', note, '',
             '| Architecture | Validation | Result |', '| --- | --- | --- |']
    lines.extend(f"| {row['platform']} | {row['check']} | {row['result']} |" for row in results)
    try:
        with open(path, 'a', encoding='utf-8') as stream:
            stream.write('\n'.join(lines) + '\n\n')
    except OSError:
        # Optional reporting must never hide a validation failure or block a local run.
        print('Warning: unable to write the optional validation summary.', file=sys.stderr, flush=True)


def qualify(expected, digest):
    results = validation_results()
    root = Path(__file__).resolve().parent.parent
    try:
        for row in results:
            label = f"{row['check']} | {row['platform']} | {expected['tag']} | {digest}"
            print(f'START: {label}', flush=True)
            try:
                run_command('bash', str(root / 'ci' / row['script']),
                            expected['image'] + '@' + digest, row['platform'])
            except Exception:
                row['result'] = 'failure'
                print(f'FAILURE: {label}', flush=True)
                raise
            row['result'] = 'success'
            print(f'SUCCESS: {label}', flush=True)
    finally:
        write_validation_summary(expected, digest, results)


def version(tag):
    match = re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", tag)
    if not match:
        raise ValueError(f"Invalid stable release tag: {tag}")
    return tuple(map(int, match.groups()))


def releases(repo):
    pages = json.loads(capture_command('gh', 'api', '--paginate', '--slurp', f'repos/{repo}/releases?per_page=100'))
    return [release for page in pages for release in page]


def may_promote(tag, records, tags):
    current = version(tag)
    candidates = tags + [r['tag_name'] for r in records]
    return not any(version(t) > current for t in candidates if re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', t))


def identity():
    repo, tag, sha = (os.environ[k] for k in ('GITHUB_REPOSITORY', 'RELEASE_TAG', 'RELEASE_SHA'))
    version(tag)
    image = image_for_repo(repo)
    if os.environ.get('IMAGE', image) != image or not re.fullmatch('[0-9a-f]{40}', sha):
        raise ValueError('Invalid release identity')
    return dict(schema=1, repository=repo, tag=tag, commit=sha, image=image)


def read_record(release, expected):
    # Enumerate assets separately: the embedded release asset list may be truncated.
    pages = json.loads(capture_command('gh', 'api', '--paginate', '--slurp',
                               f"repos/{expected['repository']}/releases/{release['id']}/assets?per_page=100"))
    assets = [a for page in pages for a in page if a['name'] == ASSET]
    if not assets:
        return None
    if len(assets) != 1 or not 0 < assets[0]['size'] <= 65536:
        raise ValueError('Invalid publication record asset')
    record = json.loads(capture_command('gh', 'api', '-H', 'Accept: application/octet-stream',
                                f"repos/{expected['repository']}/releases/assets/{assets[0]['id']}"))
    if not isinstance(record, dict) or any(record.get(k) != v for k, v in expected.items()):
        raise ValueError('Publication record identity mismatch')
    require_digest(record.get('digest'))
    if record.get('platforms') != PLATFORMS or record.get('checks') != CHECKS:
        raise ValueError('Publication record is not fully qualified')
    for key in ('run_id', 'run_attempt'):
        if not re.fullmatch('[1-9][0-9]*', str(record.get(key, ''))):
            raise ValueError('Invalid qualification run identity')
    return record


def plan(expected):
    matches = [r for r in releases(expected['repository']) if r['tag_name'] == expected['tag']]
    if len(matches) != 1:
        raise RuntimeError('Publication requires one existing GitHub release')
    release = matches[0]
    record = read_record(release, expected)
    official = registry_digest(expected['image'], expected['tag'], allow_missing=True)
    if record is None:
        if not release['draft'] or official is not None:
            raise RuntimeError('Refusing unrecorded public release or official image')
        return 'build', None
    if official is not None and official != record['digest']:
        raise RuntimeError('Official version digest differs from publication record')
    if not release['draft']:
        if official != record['digest']:
            raise RuntimeError('Public release is missing its recorded official image')
        return 'complete', record
    return 'resume', record


def output(**values):
    with open(os.environ['GITHUB_OUTPUT'], 'a', encoding='utf-8') as stream:
        for key, value in values.items():
            stream.write(f'{key}={value}\n')


def promote_ref(image, tag, digest, *, immutable=False):
    existing = registry_digest(image, tag, allow_missing=True)
    if existing == digest:
        return
    if immutable and existing is not None:
        raise RuntimeError('Refusing to replace an official version digest')
    run_command('docker', 'buildx', 'imagetools', 'create', '--tag', image + ':' + tag, image + '@' + digest)
    if registry_digest(image, tag) != digest:
        raise RuntimeError('Promotion did not preserve qualified digest')


def publish(expected):
    mode, record = plan(expected)
    if mode == 'complete':
        print('Publication already complete; no registry writes required', flush=True)
        write_validation_summary(expected, record['digest'], validation_results(),
                                 note='Already complete; no validations executed in this attempt.')
        return
    digest = require_digest(os.environ['DIGEST'])
    if record and record['digest'] != digest:
        raise RuntimeError('Retry must reuse the recorded digest')
    image = expected['image']
    if registry_digest(image, digest) != digest:
        raise RuntimeError('Candidate digest is unavailable')
    root = Path(__file__).resolve().parent.parent
    qualify(expected, digest)
    # Recheck lineage and durable state after the potentially long qualification.
    run_command('bash', str(root / 'release/verify-tag-on-main.sh'))
    current_mode, current_record = plan(expected)
    if current_mode == 'complete':
        if current_record['digest'] != digest:
            raise RuntimeError('Release changed during qualification')
        return
    if current_record and current_record['digest'] != digest:
        raise RuntimeError('Recorded digest changed during qualification')
    if current_record is None:
        record = dict(expected, digest=digest, platforms=PLATFORMS, checks=CHECKS,
                      run_id=os.environ['GITHUB_RUN_ID'], run_attempt=os.environ['GITHUB_RUN_ATTEMPT'])
        for key in ('run_id', 'run_attempt'):
            if not re.fullmatch('[1-9][0-9]*', record[key]):
                raise ValueError('Invalid qualification run identity')
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / ASSET
            path.write_text(json.dumps(record, sort_keys=True) + '\n', encoding='utf-8')
            # No --clobber: a lost successful response is recovered by the next plan.
            run_command('gh', 'release', 'upload', expected['tag'], str(path), '--repo', expected['repository'])
        _, current_record = plan(expected)
        if current_record != record:
            raise RuntimeError('Publication record was not durably stored')
    run_command('git', 'fetch', '--force', 'origin', 'refs/heads/main:refs/remotes/origin/main', '+refs/tags/*:refs/tags/*')
    tags = capture_command('git', 'tag', '--merged', 'origin/main', '--list', 'v*').splitlines()
    latest = may_promote(expected['tag'], releases(expected['repository']), tags)
    promote_ref(image, expected['tag'], digest, immutable=True)
    if latest:
        promote_ref(image, 'latest', digest)
    run_command('gh', 'release', 'edit', expected['tag'], '--repo', expected['repository'],
            '--draft=false', '--latest=' + str(latest).lower())
    print(f"Published {expected['tag']}; promoted latest={latest}")


def main():
    mode = sys.argv[1]
    if mode == 'distributed':
        image = image_for_repo(os.environ['GITHUB_REPOSITORY'])
        output(image=image, digest=registry_digest(image, 'latest'))
        return
    expected = identity()
    if mode == 'draft':
        for release in releases(expected['repository']):
            if release['tag_name'] == expected['tag']:
                if not release['draft']:
                    raise RuntimeError('Refusing to replace an already published release')
                if read_record(release, expected) is not None:
                    raise RuntimeError('Qualified assets are frozen; rerun only publish-docker to resume')
    elif mode == 'plan':
        state, record = plan(expected)
        run, attempt = os.environ['GITHUB_RUN_ID'], os.environ['GITHUB_RUN_ATTEMPT']
        if not all(re.fullmatch('[1-9][0-9]*', value) for value in (run, attempt)):
            raise ValueError('Invalid candidate run identity')
        output(mode=state, digest=record['digest'] if record else '',
               candidate=f"{expected['image']}:candidate-{expected['tag']}-{run}-{attempt}")
    elif mode == 'publish':
        publish(expected)
    else:
        raise ValueError('Unknown publication mode')


if __name__ == '__main__':
    main()
