/* SPDX-License-Identifier: GPL-2.0 */
#ifndef DUALSTEER_POLICY_H
#define DUALSTEER_POLICY_H

#include <linux/types.h>

#define DS_MODE_WRR 1U
#define DS_MODE_RTT 2U
#define DS_LEG_A 0U
#define DS_LEG_B 1U
#define DS_LEG_COUNT 2U
#define DS_MAX_CONNECTIONS 4096U
#define DS_MAX_PATHS 32768U

/* ABI v1: native-endian integers, explicit reserved fields must be zero.
 * netns_inode is stat(/proc/PID/ns/net).st_ino, NOT a netns cookie.
 * token is the local MPTCP token. The controller must remove entries at close
 * and before reusing tokens/netns inodes; these identifiers are not eternal.
 */
struct ds_conn_key {
	__u64 netns_inode;
	__u32 token;
	__u32 reserved;
};

/* Replace this entire map value atomically. Stage generation-matching path
 * entries first, commit policy last. Advance generation for every change.
 */
struct ds_policy {
	__u32 generation;
	__u32 enabled;
	__u32 mode;
	__u32 weight_a;
	__u32 weight_b;
	__u32 rtt_delta_us;
};

/* Endpoint IDs come from this connection's MPTCP PM events. ID 0 is valid.
 * The IDs are scoped to local/remote peers, not Linux interface indexes.
 */
struct ds_path_key {
	struct ds_conn_key conn;
	__u32 local_id;
	__u32 remote_id;
};

struct ds_path {
	__u32 generation;
	__u32 access;
};

/* Read-only telemetry from ds_stats_map. Counts are cumulative for the socket,
 * including across policy generations. Sample before close: release removes
 * the entry. generation/mode are the most recently observed policy (0 when
 * absent); fallback counts kernel-default calls (including reinjection and
 * calls that find no usable subflow). Allocation failure only loses
 * telemetry, never forwarding. Counters describe decisions, not packet bytes.
 */
struct ds_stats {
	__u32 generation;
	__u32 mode;
	__u64 selected_a;
	__u64 selected_b;
	__u64 fallback;
};

#endif
