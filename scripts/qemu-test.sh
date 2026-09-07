#!/usr/bin/env bash
# Return failure if the guest never reaches a verified data-plane success.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
build_root=${QEMU_BUILD:-$repo/build/qemu}
log="${QEMU_LOG:-$build_root/console.log}"
mkdir -p "$build_root"
if [[ ! -s "$build_root/kernel-out/arch/x86/boot/bzImage" ]]; then
  echo 'Build the test kernel first: make qemu-kernel' >&2
  exit 1
fi
if ! timeout --signal=TERM --kill-after=10s "${QEMU_TIMEOUT:-300}" \
    bash "$repo/scripts/qemu-run.sh" > "$log" 2>&1; then
  tail -60 "$log" >&2
  echo "QEMU failed or timed out; log: $log" >&2
  exit 1
fi
if ! grep -q '^DUALSTEER_GUEST_PASS' "$log" || \
   ! grep -q '^DUALSTEER_GUEST_EXIT=0' "$log"; then
  tail -60 "$log" >&2
  echo "Guest did not pass; log: $log" >&2
  exit 1
fi
echo "Patched-kernel guest tests passed. Log: $log"
