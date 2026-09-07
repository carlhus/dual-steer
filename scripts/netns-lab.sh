#!/usr/bin/env bash
# Two independent IP legs; no 5GC components. Run only on a disposable lab host.
set -euo pipefail
ue=ds-ue
peer=ds-peer
case "${1:-}" in
  up)
    # Refuse to reuse namespaces; do not alter a pre-existing lab.
    for ns in "$ue" "$peer"; do
      if ip netns list | awk '{print $1}' | grep -Fxq "$ns"; then
        echo "namespace $ns already exists; refusing to reuse it" >&2
        exit 1
      fi
    done
    made_ue=0
    made_peer=0
    cleanup_failure() {
      if (( made_peer )); then ip netns del "$peer"; fi
      if (( made_ue )); then ip netns del "$ue"; fi
    }
    trap cleanup_failure ERR
    ip netns add "$ue"
    made_ue=1
    ip netns add "$peer"
    made_peer=1
    ip -n "$ue" link add leg-a type veth peer name leg-a netns "$peer"
    ip -n "$ue" link add leg-b type veth peer name leg-b netns "$peer"
    for ns in "$ue" "$peer"; do
      ip -n "$ns" link set lo up
      ip -n "$ns" link set leg-a up
      ip -n "$ns" link set leg-b up
      ip netns exec "$ns" sysctl -qw net.mptcp.enabled=1
      ip -n "$ns" mptcp limits set subflows 2 add_addr_accepted 2
    done
    ip -n "$ue" addr add 10.60.1.2/24 dev leg-a
    ip -n "$ue" addr add 10.60.2.2/24 dev leg-b
    ip -n "$peer" addr add 10.60.1.1/24 dev leg-a
    ip -n "$peer" addr add 10.60.2.1/24 dev leg-b
    # Initial connection uses Leg A. Signal B from peer; UE creates that subflow.
    ip -n "$peer" mptcp endpoint add 10.60.2.1 dev leg-b id 2 signal
    ip netns exec "$ue" tc qdisc replace dev leg-a root netem delay 5ms
    ip netns exec "$ue" tc qdisc replace dev leg-b root netem delay 40ms
    trap - ERR
    echo "Created ds-ue <-> ds-peer with leg-a and leg-b; default scheduler unchanged."
    ;;
  down)
    # Explicit teardown only; refuses to kill active applications implicitly.
    for ns in "$ue" "$peer"; do
      if ip netns list | awk '{print $1}' | grep -Fxq "$ns"; then
        if [[ -n "$(ip netns pids "$ns")" ]]; then
          echo "namespace $ns has running processes; stop them before teardown" >&2
          exit 1
        fi
      fi
    done
    for ns in "$peer" "$ue"; do
      if ip netns list | awk '{print $1}' | grep -Fxq "$ns"; then ip netns del "$ns"; fi
    done
    ;;
  *) echo "usage: sudo bash scripts/netns-lab.sh up|down" >&2; exit 2 ;;
esac
