#!/usr/bin/env bash
# Build a disposable userspace for the patched-kernel test VM.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
root="$repo/build/qemu-root"
mkdir -p "$repo/build"
if [[ -e "$root" && ! -f "$root/.dualsteer-initramfs" ]]; then
  echo "Refusing to replace unmarked directory: $root" >&2
  exit 1
fi
rm -rf "$root"
mkdir -p "$root"/{bin,sbin,usr/bin,usr/sbin,proc,sys,dev,tmp,run,etc,work,root}
touch "$root/.dualsteer-initramfs"
cp "$(command -v busybox)" "$root/bin/busybox"
for app in $(busybox --list); do
  [[ "$app" == busybox ]] || ln -s busybox "$root/bin/$app"
done

# Copy only trusted, installed host binaries and their runtime dependencies.
copy_libs() {
  local binary=$1 dependency
  while read -r dependency; do
    [[ -n "$dependency" && -f "$dependency" ]] || continue
    mkdir -p "$root$(dirname "$dependency")"
    cp -L "$dependency" "$root$dependency"
  done < <(ldd "$binary" 2>/dev/null | awk '/=> \/|^[[:space:]]*\// {for (i=1;i<=NF;i++) if ($i ~ /^\//) print $i}' || true)
}
copy_bin() {
  local source=$1 target=$2
  mkdir -p "$root$(dirname "$target")"
  rm -f "$root$target"
  cp -L "$source" "$root$target"
  copy_libs "$source"
}
copy_bin "$(command -v bash)" /bin/bash
for binary in ip tc ss stdbuf timeout; do
  copy_bin "$(command -v "$binary")" "/usr/bin/$binary"
done
# Ubuntu's /usr/sbin/bpftool may be a kernel-version dispatch shell script.
bpftool_bin=${BPFTOOL_BIN:-/usr/lib/linux-tools/$(uname -r)/bpftool}
if [[ ! -x "$bpftool_bin" ]]; then bpftool_bin=$(command -v bpftool); fi
if [[ $(file -Lb "$bpftool_bin") != *ELF* ]]; then
  echo 'Set BPFTOOL_BIN to an actual bpftool ELF binary' >&2
  exit 1
fi
copy_bin "$bpftool_bin" /usr/bin/bpftool
copy_bin "$(command -v python3)" /usr/bin/python3
python_lib=$(python3 -c 'import sysconfig; print(sysconfig.get_path("stdlib"))')
mkdir -p "$root$(dirname "$python_lib")"
cp -a "$python_lib" "$root$python_lib"
while IFS= read -r -d '' extension; do copy_libs "$extension"; done \
  < <(find "$python_lib/lib-dynload" -name '*.so' -print0)
for libdir in /usr/libexec/coreutils /usr/lib/x86_64-linux-gnu/tc /usr/lib/tc; do
  if [[ -d "$libdir" ]]; then
    mkdir -p "$root$(dirname "$libdir")"
    cp -a "$libdir" "$root$libdir"
  fi
done
printf 'root:x:0:0:root:/root:/bin/bash\n' > "$root/etc/passwd"
printf 'root:x:0:\n' > "$root/etc/group"
printf '127.0.0.1 localhost\n' > "$root/etc/hosts"
cat > "$root/init" <<'INIT'
#!/bin/sh
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
ln -s /proc/self/fd /dev/fd
ln -s /proc/self/fd/0 /dev/stdin
ln -s /proc/self/fd/1 /dev/stdout
ln -s /proc/self/fd/2 /dev/stderr
mkdir -p /dev/pts /sys/fs/bpf /run/lock
mount -t devpts devpts /dev/pts
mount -t bpf bpf /sys/fs/bpf
mount -t 9p -o trans=virtio,version=9p2000.L,msize=262144 dualsteer /work
if ! test -f /work/scripts/guest-verify.sh; then
  echo 'DUALSTEER_GUEST_FAIL: cannot mount test repository'
  poweroff -f
fi
cd /work
bash scripts/guest-verify.sh
result=$?
echo "DUALSTEER_GUEST_EXIT=$result"
sync
poweroff -f
INIT
chmod +x "$root/init"
mkdir -p "$repo/build/qemu"
(cd "$root" && find . -print0 | cpio --null -o --format=newc --owner=0:0 2>/dev/null) \
  | gzip -1 > "$repo/build/qemu/initramfs.cpio.gz"
echo "$repo/build/qemu/initramfs.cpio.gz"
