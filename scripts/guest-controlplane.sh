#!/usr/bin/env bash
# Runs only inside the disposable QEMU guest, using the actual patched NFs.
set -euo pipefail
if ! grep -qw 'dualsteer.guest=1' /proc/cmdline; then
    echo 'Refusing to run outside the DualSteer test VM' >&2
    exit 1
fi
cd /work
mkdir -p build/guest-controlplane
exec > >(tee build/guest-controlplane/verification.log) 2>&1
echo "Control-plane guest kernel: $(uname -r)"
ulimit -l unlimited
python3 - <<'PY'
import datetime
import gzip
import hashlib
import json
from pathlib import Path
import platform

artifacts = [Path(name) for name in (
    'build/dualsteer.bpf.o', 'build/dualsteer-agent', 'build/pcf-research', 'build/smf-research',
    'controlplane/protocol.go', 'scripts/verify-controlplane.py', 'scripts/guest-controlplane.sh',
    'integration/free5gc/pins.json')]
artifacts += sorted(Path('integration/free5gc').rglob('*.patch'))
manifest = {'recorded_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'kernel': platform.release(),
            'kernel_source_commit': Path('build/qemu/source-commit').read_text().strip(),
            'software_pins': json.loads(Path('integration/free5gc/pins.json').read_text()),
            'sha256': {}}
for path in artifacts:
    with path.open('rb') as stream:
        manifest['sha256'][str(path)] = hashlib.file_digest(stream, 'sha256').hexdigest()
Path('build/guest-controlplane/manifest.json').write_text(json.dumps(manifest, indent=2)+'\n')
with gzip.open('/proc/config.gz', 'rb') as stream:
    Path('build/guest-controlplane/kernel.config').write_bytes(stream.read())
PY
bpftool version
bpftool struct_ops register build/dualsteer.bpf.o
bpftool -j struct_ops show > build/guest-controlplane/struct-ops.json
bpftool -j map show > build/guest-controlplane/maps.json
bpftool -j prog show > build/guest-controlplane/programs.json
read -r policy_id path_id stats_id < <(python3 - <<'PY'
import json
from pathlib import Path
maps = json.loads(Path('build/guest-controlplane/maps.json').read_text())
def map_id(name):
    found = [entry['id'] for entry in maps if entry['name'] == name]
    if len(found) != 1:
        raise SystemExit(f'Expected one {name}, found {found}')
    return found[0]
print(map_id('ds_policy_map'), map_id('ds_path_map'), map_id('ds_stats_map'))
PY
)
python3 scripts/verify-controlplane.py \
    --policy-map "id:$policy_id" --path-map "id:$path_id" --stats-map "id:$stats_id" \
    --output /work/build/runtime-controlplane
echo DUALSTEER_CONTROLPLANE_PASS
echo DUALSTEER_GUEST_PASS
