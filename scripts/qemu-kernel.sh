#!/usr/bin/env bash
# Build the paper's kernel in an isolated directory; never installs a host kernel.
set -euo pipefail
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source_repo=${KERNEL_SRC:-/home/ubuntu/atsss/ATSSS-UE/mptcp_net-next}
build_root=${QEMU_BUILD:-$repo/build/qemu}
kernel_src=$build_root/kernel-src
kernel_out=$build_root/kernel-out
jobs=${JOBS:-6}
mkdir -p "$build_root" "$kernel_out"
if [[ ! -e $kernel_src/Makefile ]]; then
    mkdir -p "$kernel_src"
    git -C "$source_repo" rev-parse HEAD > "$build_root/source-commit"
    # Git archive restores executable bits and symlinks damaged in the source
    # checkout, without changing that checkout or its Git administration.
    git -C "$source_repo" archive HEAD | tar -x -C "$kernel_src"
fi
if ! rg -q 'int bpf_mptcp_sched_default' "$kernel_src/net/mptcp/bpf.c"; then
    (cd "$kernel_src" && patch -p1 < "$repo/bpf/mptcp-default-kfunc.patch")
fi
if [[ ! -f $kernel_out/.config ]]; then
    make -C "$kernel_src" O="$kernel_out" tinyconfig
fi
# Reapply the recipe for cached builds as well, so new requirements migrate.
cfg=("$kernel_src/scripts/config" --file "$kernel_out/.config")
for opt in 64BIT SMP MULTIUSER PRINTK BUG EXPERT SYSCTL SYSVIPC MODULES \
        POSIX_TIMERS FUTEX EPOLL EVENTFD SIGNALFD TIMERFD ADVISE_SYSCALLS \
        NAMESPACES UTS_NS IPC_NS USER_NS PID_NS NET_NS \
        BINFMT_ELF BINFMT_SCRIPT BLK_DEV_INITRD RD_GZIP \
        PROC_FS SYSFS TMPFS SHMEM DEVTMPFS DEVTMPFS_MOUNT FILE_LOCKING \
        TTY SERIAL_8250 SERIAL_8250_CONSOLE UNIX98_PTYS \
        PCI PCI_MSI VIRTIO_MENU VIRTIO VIRTIO_PCI ACPI \
        NET UNIX PACKET INET IPV6 NETDEVICES NET_CORE VETH VIRTIO_NET \
        INET_DIAG INET_TCP_DIAG INET_MPTCP_DIAG MPTCP MPTCP_IPV6 \
        NET_SCHED NET_SCH_NETEM NET_SCH_FQ NET_SCH_INGRESS NET_CLS_ACT \
        BPF BPF_SYSCALL BPF_JIT BPF_JIT_ALWAYS_ON BPF_EVENTS \
        PERF_EVENTS FTRACE KPROBES KPROBE_EVENTS UPROBES UPROBE_EVENTS \
        KALLSYMS KALLSYMS_ALL FUNCTION_TRACER DYNAMIC_FTRACE \
        CGROUPS CGROUP_BPF NETWORK_FILESYSTEMS NET_9P NET_9P_VIRTIO 9P_FS \
    DEBUG_KERNEL DEBUG_INFO_DWARF4 DEBUG_INFO_BTF IKCONFIG IKCONFIG_PROC; do
    "${cfg[@]}" -e "$opt"
done
"${cfg[@]}" -d DEBUG_INFO_NONE -d DEBUG_INFO_REDUCED -d DEBUG_INFO_SPLIT \
    -d DEBUG_INFO_DWARF_TOOLCHAIN_DEFAULT -d WERROR \
    --set-str LOCALVERSION '-dualsteer-test' --set-val NR_CPUS 8
make -C "$kernel_src" O="$kernel_out" olddefconfig
for opt in CONFIG_MPTCP CONFIG_BPF_SYSCALL CONFIG_BPF_JIT CONFIG_DEBUG_INFO_BTF \
    CONFIG_NET_NS CONFIG_VETH CONFIG_NET_SCH_NETEM CONFIG_9P_FS CONFIG_NET_9P_VIRTIO \
    CONFIG_FILE_LOCKING CONFIG_KALLSYMS; do
    if ! rg -q "^$opt=y$" "$kernel_out/.config"; then
        echo "Required kernel option missing: $opt" >&2
        exit 1
    fi
done
make -C "$kernel_src" O="$kernel_out" -j"$jobs" bzImage
echo "Kernel ready: $kernel_out/arch/x86/boot/bzImage"
