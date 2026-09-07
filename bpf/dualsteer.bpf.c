// SPDX-License-Identifier: GPL-2.0
/* DualSteer policy adapter for the ATSSS MPTCP struct_ops ABI.
 * API provenance: tools/testing/selftests/bpf/progs/mptcp_bpf_rr.c,
 * mptcp_bpf_rr_quota.c, mptcp_bpf_rtt.c and mptcp_bpf_burst.c
 * in ATSSS-UE/mptcp_net-next (Linux 6.6-rc2 fork).
 * No original scheduler or deployment-specific address is modified.
 */
#include <linux/bpf.h>
#include "bpf_tcp_helpers.h"
#include "dualsteer_select.h"

char _license[] SEC("license") = "GPL";

/* CO-RE flavors extend the fork's deliberately compact helper declarations. */
struct ns_common___ds { unsigned int inum; } __attribute__((preserve_access_index));
struct net___ds { struct ns_common___ds ns; } __attribute__((preserve_access_index));
typedef struct { struct net___ds *net; } possible_net_t___ds;
struct sock_common___ds {
	possible_net_t___ds skc_net;
} __attribute__((preserve_access_index));
struct mptcp_subflow_context___ds {
	__u8 local_id;
	__u8 remote_id;
} __attribute__((preserve_access_index));

extern bool mptcp_subflow_active(struct mptcp_subflow_context *subflow) __ksym;
extern bool tcp_stream_memory_free(const struct sock *sk, int wake) __ksym;
extern void mptcp_set_timeout(struct sock *sk) __ksym;
extern __u64 mptcp_wnd_end(const struct mptcp_sock *msk) __ksym;
/* Mandatory kernel bridge supplied in mptcp-default-kfunc.patch. */
extern int bpf_mptcp_sched_default(struct mptcp_sock *msk,
				 struct mptcp_sched_data *data) __ksym;

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, DS_MAX_CONNECTIONS);
	__type(key, struct ds_conn_key);
	__type(value, struct ds_policy);
} ds_policy_map SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, DS_MAX_PATHS);
	__type(key, struct ds_path_key);
	__type(value, struct ds_path);
} ds_path_map SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, DS_MAX_CONNECTIONS);
	__type(key, struct ds_conn_key);
	__type(value, struct ds_stats);
} ds_stats_map SEC(".maps");

/* Socket-local state cannot leak credit/history across token reuse. The MPTCP
 * socket lock serializes scheduler calls in this kernel. Controller never
 * writes this map; socket destruction automatically frees it.
 */
struct ds_socket_state {
	struct ds_state selection;
	/* The kernel may clear msk->token before scheduler release. */
	struct ds_conn_key stats_key;
	/* Per-call scratch lives here to bound old-verifier state exploration.
	 * The owning MPTCP socket lock prevents concurrent scheduler calls.
	 */
	struct ds_candidate candidates[2];
	struct ds_candidate backups[2];
};

struct {
	__uint(type, BPF_MAP_TYPE_SK_STORAGE);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, int);
	__type(value, struct ds_socket_state);
} ds_state_map SEC(".maps");

static __always_inline void ds_connection_key(struct mptcp_sock *msk,
					      struct ds_conn_key *key)
{
	key->token = BPF_CORE_READ(msk, token);
	key->netns_inode = BPF_CORE_READ((struct sock_common___ds *)msk,
				       skc_net.net, ns.inum);
}

static __always_inline void ds_record(const struct ds_conn_key *key,
				      const struct ds_policy *policy, int leg)
{
	struct ds_stats *stats;

	if (!key->netns_inode)
		return;
	stats = bpf_map_lookup_elem(&ds_stats_map, key);
	if (!stats) {
		struct ds_stats initial = {};

		bpf_map_update_elem(&ds_stats_map, key, &initial, BPF_NOEXIST);
		stats = bpf_map_lookup_elem(&ds_stats_map, key);
		if (!stats)
			return;
	}
	stats->generation = policy->generation;
	stats->mode = policy->mode;
	if (leg == DS_LEG_A)
		stats->selected_a++;
	else if (leg == DS_LEG_B)
		stats->selected_b++;
	else
		stats->fallback++;
}

static __always_inline int ds_schedule(struct mptcp_sock *msk,
				       struct mptcp_sched_data *data)
{
	struct ds_conn_key key = {};
	struct ds_policy *mapped, policy = {};
	struct ds_candidate *candidates, *backups;
	struct ds_socket_state *storage = 0;
	struct ds_state *state;
	struct mptcp_subflow_context *subflow;
	__u64 window;
	int chosen, active = 0;

	ds_connection_key(msk, &key);
	if (!key.netns_inode)
		goto fallback;
	storage = bpf_sk_storage_get(&ds_state_map, msk, 0,
				     BPF_LOCAL_STORAGE_GET_F_CREATE);
	if (storage) {
		if (storage->stats_key.netns_inode &&
		    (storage->stats_key.netns_inode != key.netns_inode ||
		     storage->stats_key.token != key.token)) {
			bpf_map_delete_elem(&ds_stats_map, &storage->stats_key);
			storage->selection.generation = 0;
		}
		storage->stats_key = key;
	}
	mapped = bpf_map_lookup_elem(&ds_policy_map, &key);
	if (!mapped)
		goto fallback;
	/* HASH replacement publishes a complete immutable value. */
	policy = *mapped;
	/* Keep kernel reinjection queue/stale/backup handling exactly intact. */
	if (data->reinject)
		goto fallback;
	if (!ds_policy_valid(&policy) || !policy.enabled)
		goto fallback;
	if (!storage)
		goto fallback;
	state = &storage->selection;
	candidates = storage->candidates;
	backups = storage->backups;
	candidates[0] = (struct ds_candidate){DS_NONE, 0};
	candidates[1] = (struct ds_candidate){DS_NONE, 0};
	backups[0] = (struct ds_candidate){DS_NONE, 0};
	backups[1] = (struct ds_candidate){DS_NONE, 0};

	for (int i = 0; i < MPTCP_SUBFLOWS_MAX; i++) {
		struct ds_path_key path_key = {.conn = key};
		struct ds_path *path;
		struct sock *ssk;
		__u32 rtt, access;

		if (i >= data->subflows)
			break;
		subflow = bpf_mptcp_subflow_ctx_by_pos(data, i);
		if (!subflow || !mptcp_subflow_active(subflow))
			continue;
		/* Preserve backup preference even for active unclassified paths. */
		if (!BPF_CORE_READ_BITFIELD_PROBED(subflow, backup))
			active = 1;
		ssk = mptcp_subflow_tcp_sock(subflow);
		if (!ssk || BPF_CORE_READ(ssk, sk_wmem_queued) >=
			    BPF_CORE_READ(ssk, sk_sndbuf) ||
		    !tcp_stream_memory_free(ssk, 0))
			continue;
		path_key.local_id = BPF_CORE_READ((struct mptcp_subflow_context___ds *)subflow, local_id);
		path_key.remote_id = BPF_CORE_READ((struct mptcp_subflow_context___ds *)subflow, remote_id);
		path = bpf_map_lookup_elem(&ds_path_map, &path_key);
		if (!path || path->generation != policy.generation)
			continue;
		access = path->access;
		if (access >= DS_LEG_COUNT)
			continue;
		rtt = BPF_CORE_READ(tcp_sk(ssk), srtt_us) >> 3;
		if (policy.mode == DS_MODE_RTT && !rtt)
			continue;
		/* Literal indexes also avoid the old verifier losing the access
		 * bound across a spilled scalar and a probe-read helper call.
		 */
		if (BPF_CORE_READ_BITFIELD_PROBED(subflow, backup)) {
			if (access == DS_LEG_A)
				ds_offer(&backups[0], i, rtt);
			else if (access == DS_LEG_B)
				ds_offer(&backups[1], i, rtt);
		} else {
			if (access == DS_LEG_A)
				ds_offer(&candidates[0], i, rtt);
			else if (access == DS_LEG_B)
				ds_offer(&candidates[1], i, rtt);
		}
	}
	if (!active) {
		candidates[0] = backups[0];
		candidates[1] = backups[1];
	}
	chosen = ds_select(&policy, state, candidates);
	if (chosen < 0 || chosen >= MPTCP_SUBFLOWS_MAX)
		goto fallback;
	subflow = bpf_mptcp_subflow_ctx_by_pos(data, chosen);
	if (!subflow)
		goto fallback;
	window = mptcp_wnd_end(msk) - BPF_CORE_READ(msk, snd_nxt);
	msk->snd_burst = window < 65428 ? window : 65428;
	mptcp_set_timeout((struct sock *)msk);
	mptcp_subflow_set_scheduled(subflow, true);
	ds_record(&key, &policy, state->previous_access);
	return 0;

fallback:
	if (storage)
		ds_record(&key, &policy, DS_NONE);
	return bpf_mptcp_sched_default(msk, data);
}

SEC("struct_ops/ds_init")
void BPF_PROG(ds_init, struct mptcp_sock *msk)
{
	bpf_sk_storage_get(&ds_state_map, msk, 0, BPF_LOCAL_STORAGE_GET_F_CREATE);
}

SEC("struct_ops/ds_release")
void BPF_PROG(ds_release, struct mptcp_sock *msk)
{
	struct ds_socket_state *storage;

	storage = bpf_sk_storage_get(&ds_state_map, msk, 0, 0);
	if (storage && storage->stats_key.netns_inode)
		bpf_map_delete_elem(&ds_stats_map, &storage->stats_key);
	bpf_sk_storage_delete(&ds_state_map, msk);
}

int BPF_STRUCT_OPS(ds_wrr_get_subflow, struct mptcp_sock *msk,
		   struct mptcp_sched_data *data)
{
	return ds_schedule(msk, data);
}

int BPF_STRUCT_OPS(ds_rtt_get_subflow, struct mptcp_sock *msk,
		   struct mptcp_sched_data *data)
{
	return ds_schedule(msk, data);
}

/* Both names accept runtime mode changes through the same policy map. */
SEC(".struct_ops")
struct mptcp_sched_ops ds_wrr = {
	.init = (void *)ds_init,
	.release = (void *)ds_release,
	.get_subflow = (void *)ds_wrr_get_subflow,
	.name = "bpf_ds_wrr",
};

SEC(".struct_ops")
struct mptcp_sched_ops ds_rtt = {
	.init = (void *)ds_init,
	.release = (void *)ds_release,
	.get_subflow = (void *)ds_rtt_get_subflow,
	.name = "bpf_ds_rtt",
};
