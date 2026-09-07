#!/usr/bin/env bash
# Runs as PID 1's child inside the disposable QEMU guest, never on the host.
set -euo pipefail
if ! grep -q 'dualsteer.guest=1' /proc/cmdline; then
  echo 'Refusing to run outside the DualSteer test VM' >&2
  exit 1
fi
cd /work
mkdir -p build/guest-results
exec > >(tee build/guest-results/verification.log) 2>&1
echo "Guest kernel: $(uname -r)"
ulimit -l unlimited
bpftool version
python3 - <<'PY'
import datetime
import gzip
import hashlib
import json
from pathlib import Path
import platform

artifacts = ['build/dualsteer.bpf.o', 'build/dualsteer-agent',
             'bpf/dualsteer.bpf.c', 'bpf/mptcp-default-kfunc.patch',
             'include/dualsteer_policy.h', 'include/dualsteer_select.h']
manifest = {'recorded_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'kernel': platform.release(),
            'kernel_source_commit': Path('build/qemu/source-commit').read_text().strip(),
            'sha256': {}}
for name in artifacts:
    with open(name, 'rb') as f:
        manifest['sha256'][name] = hashlib.file_digest(f, 'sha256').hexdigest()
Path('build/guest-results/manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
with gzip.open('/proc/config.gz', 'rb') as f:
    Path('build/guest-results/kernel.config').write_bytes(f.read())
PY
bpftool struct_ops register build/dualsteer.bpf.o
bpftool -j struct_ops show > build/guest-results/struct-ops.json
bpftool -j map show > build/guest-results/maps.json
bpftool -j prog show > build/guest-results/programs.json
read -r policy_id path_id stats_id < <(python3 - <<'PY'
import json
from pathlib import Path
maps = json.loads(Path('build/guest-results/maps.json').read_text())
def map_id(name):
    found = [m['id'] for m in maps if m['name'] == name]
    if len(found) != 1:
        raise SystemExit(f'Expected exactly one {name} map, found {found}')
    return found[0]
print(map_id('ds_policy_map'), map_id('ds_path_map'), map_id('ds_stats_map'))
PY
)
python3 scripts/verify-dataplane.py --bpf \
  --policy-map "id:$policy_id" --path-map "id:$path_id" --stats-map "id:$stats_id" \
  --output /work/build/runtime-bpf
python3 - "$policy_id" "$path_id" "$stats_id" <<'PY'
import json
from pathlib import Path
import subprocess
import sys
import time

for attempt in range(30):
    remaining = {
        name: json.loads(subprocess.check_output(
            ['bpftool', '-j', 'map', 'dump', 'id', map_id], text=True))
        for name, map_id in zip(('policy', 'paths', 'stats'), sys.argv[1:])
    }
    if not any(remaining.values()):
        break
    time.sleep(.2)
Path('build/guest-results/maps-after-cleanup.json').write_text(
    json.dumps(remaining, indent=2) + '\n')
if any(remaining.values()):
    raise SystemExit('Connection cleanup left map entries: ' + json.dumps(remaining))
print('Connection cleanup verified: policy, path and stats maps empty')
PY
echo DUALSTEER_GUEST_PASS
