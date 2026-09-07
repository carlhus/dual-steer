#!/usr/bin/env python3
"""Prepare pinned, isolated NFs; export/review patches without touching original trees."""
import argparse
import json
import pathlib
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
ARTIFACTS = ROOT / 'integration/free5gc'
PINS = json.loads((ARTIFACTS / 'pins.json').read_text())
DEST = ROOT / 'build/free5gc-v4.2.3'


def git(directory, *args, capture=False):
    return subprocess.run(['git', '-C', str(directory), *args], check=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def clone(source, target, commit):
    subprocess.run(['git', 'clone', '--no-hardlinks', '--no-checkout', str(source), str(target)], check=True)
    git(target, 'checkout', '--detach', commit)


def prepare(source):
    # Reusing an existing tree is explicit: never reset or clean it.
    if DEST.exists():
        for name, directory in [('core', DEST), ('pcf', DEST / 'NFs/pcf'), ('smf', DEST / 'NFs/smf')]:
            if git(directory, 'rev-parse', 'HEAD', capture=True).decode().strip() != PINS[name]['commit']:
                raise SystemExit(f'{directory}: unexpected HEAD; refusing to change existing checkout')
        for name in ('pcf', 'smf'):
            git(DEST / 'NFs' / name, 'apply', '--reverse', '--check', str(ARTIFACTS / f'{name}.patch'))
        print(f'Existing pinned checkout already contains research patches: {DEST}')
        return
    DEST.parent.mkdir(parents=True, exist_ok=True)
    clone(source or PINS['core']['url'], DEST, PINS['core']['commit'])
    for name in ('pcf', 'smf'):
        origin = pathlib.Path(source) / 'NFs' / name if source else PINS[name]['url']
        clone(origin, DEST / 'NFs' / name, PINS[name]['commit'])
        git(DEST / 'NFs' / name, 'apply', str(ARTIFACTS / f'{name}.patch'))
    print(DEST)


def export():
    for name in ('pcf', 'smf'):
        directory = DEST / 'NFs' / name
        # Temporary index includes new files while preserving the developer index.
        import os
        with tempfile.TemporaryDirectory() as temporary:
            environment = dict(os.environ, GIT_INDEX_FILE=str(pathlib.Path(temporary) / 'index'))
            for args in [('read-tree', 'HEAD'), ('add', '-A')]:
                subprocess.run(['git', '-C', str(directory), *args], env=environment, check=True)
            patch = subprocess.check_output(['git', '-C', str(directory), 'diff', '--cached', '--binary', 'HEAD'], env=environment)
        (ARTIFACTS / f'{name}.patch').write_bytes(patch)
        print(f'{name}: exported {len(patch)} bytes')


def verify():
    for name in ('pcf', 'smf'):
        with tempfile.TemporaryDirectory(prefix=f'dualsteer-{name}-') as temporary:
            directory = pathlib.Path(temporary) / name
            clone(DEST / 'NFs' / name, directory, PINS[name]['commit'])
            git(directory, 'apply', '--check', str(ARTIFACTS / f'{name}.patch'))
            git(directory, 'apply', str(ARTIFACTS / f'{name}.patch'))
            print(f'{name}: patch applies cleanly at {PINS[name]["commit"]}')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['prepare', 'export', 'verify'])
    parser.add_argument('--source', help='optional existing free5GC checkout to clone read-only instead of GitHub')
    args = parser.parse_args()
    if args.action == 'prepare':
        prepare(args.source)
    elif args.action == 'export':
        export()
    else:
        verify()
