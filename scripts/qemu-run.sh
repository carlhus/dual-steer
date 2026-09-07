#!/usr/bin/env bash
# A disposable VM with no network devices. All test links live inside its netns.
set -euo pipefail
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=${QEMU_BUILD:-$repo/build/qemu}
initramfs=${INITRAMFS:-$build_root/initramfs.cpio.gz}
kernel=$build_root/kernel-out/arch/x86/boot/bzImage
accel=tcg
[[ ! -r /dev/kvm || ! -w /dev/kvm ]] || accel=kvm
exec qemu-system-x86_64 -machine "accel=$accel" -cpu max \
    -m "${QEMU_MEMORY:-2048}" -smp "${QEMU_CPUS:-2}" \
    -kernel "$kernel" -initrd "$initramfs" \
    -append 'console=ttyS0 earlyprintk=serial panic=-1 nokaslr dualsteer.guest=1' \
    -nographic -no-reboot -nic none \
    -virtfs "local,path=$repo,mount_tag=dualsteer,security_model=none,id=ds" "$@"
