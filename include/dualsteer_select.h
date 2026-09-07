/* SPDX-License-Identifier: GPL-2.0 */
#ifndef DUALSTEER_SELECT_H
#define DUALSTEER_SELECT_H

#include "dualsteer_policy.h"

#define DS_INLINE static __inline __attribute__((always_inline))
#define DS_NONE (-1)

/* One candidate per access; availability is checked by the BPF adapter.
 * Multiple subflows on one access do not multiply that access's weight.
 */
struct ds_candidate {
	int position;
	__u32 rtt_us;
};

struct ds_state {
	__u32 generation;
	__u32 mode;
	__s32 credit[DS_LEG_COUNT];
	__u32 available_mask;
	__s32 previous_access;
};

DS_INLINE int ds_policy_valid(const struct ds_policy *p)
{
	return p->generation != 0 && p->enabled <= 1 &&
		(p->mode == DS_MODE_WRR || p->mode == DS_MODE_RTT) &&
		p->weight_a <= 100 && p->weight_b <= 100 &&
		p->weight_a + p->weight_b == 100;
}

DS_INLINE void ds_reset(struct ds_state *s, const struct ds_policy *p)
{
	s->generation = p->generation;
	s->mode = p->mode;
	s->credit[0] = 0;
	s->credit[1] = 0;
	s->available_mask = 0;
	s->previous_access = DS_NONE;
}

DS_INLINE void ds_offer(struct ds_candidate *c, int position, __u32 rtt_us)
{
#ifdef __bpf__
	/* LLVM can fold an aligned stack-array GEP + member offset into OR on
	 * a pointer, which the BPF verifier rejects. Keep the pointer opaque to
	 * that optimization; the verifier still sees its original bounds.
	 */
	__asm__ __volatile__("" : "+r"(c));
#endif
	if (c->position == DS_NONE ||
	    (rtt_us && (!c->rtt_us || rtt_us < c->rtt_us))) {
		c->position = position;
		c->rtt_us = rtt_us;
	}
}

/* Returns the selected data->contexts position or DS_NONE (real fallback).
 * This exact function is used in BPF and native tests. WRR weights schedule
 * decisions, not a guaranteed byte/packet ratio. RTT delta is hysteresis:
 * switch only when the improvement is strictly greater than delta.
 */
DS_INLINE int ds_select(const struct ds_policy *p, struct ds_state *s,
		       const struct ds_candidate c[DS_LEG_COUNT])
{
	__u32 mask = 0;
	__u32 rtt_a, rtt_b;
	int a, b, pick, pos_a, pos_b;

#ifdef __bpf__
	__asm__ __volatile__("" : "+r"(c));
#endif

	if (!ds_policy_valid(p) || !p->enabled)
		return DS_NONE;
	if (s->generation != p->generation || s->mode != p->mode)
		ds_reset(s, p);

	pos_a = c[0].position;
	pos_b = c[1].position;
	rtt_a = c[0].rtt_us;
	rtt_b = c[1].rtt_us;
	a = pos_a != DS_NONE;
	b = pos_b != DS_NONE;
	if (p->mode == DS_MODE_RTT) {
		a = a && rtt_a != 0;
		b = b && rtt_b != 0;
		if (!a && !b)
			return DS_NONE;
		pick = !a ? 1 : !b ? 0 : rtt_b < rtt_a;
		if (p->rtt_delta_us && a && b &&
		    (s->previous_access == 0 || s->previous_access == 1)) {
			int prev = s->previous_access;
			__u32 previous_rtt = prev == 0 ? rtt_a : rtt_b;
			__u32 chosen_rtt = pick == 0 ? rtt_a : rtt_b;
			/* Difference cannot underflow: pick is the minimum. */
			if (previous_rtt - chosen_rtt <= p->rtt_delta_us)
				pick = prev;
		}
	} else {
		a = a && p->weight_a != 0;
		b = b && p->weight_b != 0;
		mask = (__u32)a | ((__u32)b << 1);
		if (mask != s->available_mask) {
			s->credit[0] = 0;
			s->credit[1] = 0;
			s->available_mask = mask;
		}
		if (!mask)
			return DS_NONE;
		if (a)
			s->credit[0] += p->weight_a;
		if (b)
			s->credit[1] += p->weight_b;
		pick = !a ? 1 : !b ? 0 : s->credit[1] > s->credit[0];
		if (pick == 0)
			s->credit[0] -= (a ? p->weight_a : 0) + (b ? p->weight_b : 0);
		else
			s->credit[1] -= (a ? p->weight_a : 0) + (b ? p->weight_b : 0);
	}
	s->previous_access = pick;
	return pick == 0 ? pos_a : pos_b;
}

#endif
