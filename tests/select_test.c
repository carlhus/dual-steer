// SPDX-License-Identifier: GPL-2.0
#include <assert.h>
#include <limits.h>
#include <stddef.h>
#include <stdio.h>
#include "dualsteer_select.h"

_Static_assert(sizeof(struct ds_conn_key) == 16, "conn ABI");
_Static_assert(sizeof(struct ds_policy) == 24, "policy ABI");
_Static_assert(sizeof(struct ds_path_key) == 24, "path key ABI");
_Static_assert(sizeof(struct ds_path) == 8, "path ABI");
_Static_assert(sizeof(struct ds_stats) == 32, "stats ABI");
_Static_assert(offsetof(struct ds_stats, selected_a) == 8, "stats counter offset");
_Static_assert(offsetof(struct ds_policy, rtt_delta_us) == 20, "delta offset");
_Static_assert(offsetof(struct ds_path_key, local_id) == 16, "endpoint offset");

static struct ds_policy policy(unsigned int mode, unsigned int a)
{
	return (struct ds_policy){1, 1, mode, a, 100 - a, 0};
}

static void test_all_ratios_and_generations(void)
{
	const struct ds_candidate c[2] = {{3, 20000}, {6, 40000}};
	struct ds_state s = {0};
	/* Exercise every valid integer ratio, hot updates on existing state,
	 * and a long run to detect drift, credit accumulation and overflow.
	 */
	for (unsigned int a = 0; a <= 100; a++) {
		struct ds_policy p = policy(DS_MODE_WRR, a);
		int counts[2] = {0};
		p.generation = a + 1;
		for (int i = 0; i < 10000; i++) {
			int chosen = ds_select(&p, &s, c);
			assert(chosen == 3 || chosen == 6);
			counts[chosen == 6]++;
			assert(s.credit[0] >= -100 && s.credit[0] <= 100);
			assert(s.credit[1] >= -100 && s.credit[1] <= 100);
		}
		assert(counts[0] == (int)a * 100);
		assert(counts[1] == (int)(100 - a) * 100);
	}
}

static void test_failure_recovery(void)
{
	struct ds_policy p = policy(DS_MODE_WRR, 70);
	struct ds_state s = {0};
	struct ds_candidate c[2] = {{0, 10}, {1, 20}};
	assert(ds_select(&p, &s, c) == 0);
	c[0].position = DS_NONE;
	for (int i = 0; i < 1000; i++)
		assert(ds_select(&p, &s, c) == 1);
	c[0].position = 0;
	int a = 0;
	for (int i = 0; i < 100; i++)
		a += ds_select(&p, &s, c) == 0;
	assert(a == 70);
	c[0].position = DS_NONE;
	c[1].position = DS_NONE;
	assert(ds_select(&p, &s, c) == DS_NONE);
	/* Zero-weight access never receives policy traffic when positive access
	 * is unavailable; adapter must call real default fallback. */
	p.weight_a = 100;
	p.weight_b = 0;
	p.generation++;
	c[1].position = 1;
	assert(ds_select(&p, &s, c) == DS_NONE);
}

static void test_rtt_and_hysteresis(void)
{
	struct ds_policy p = policy(DS_MODE_RTT, 70);
	struct ds_state s = {0};
	struct ds_candidate c[2] = {{0, 0}, {1, 0}};
	assert(ds_select(&p, &s, c) == DS_NONE);
	c[1].rtt_us = 40000;
	assert(ds_select(&p, &s, c) == 1);
	c[0].rtt_us = 20000;
	assert(ds_select(&p, &s, c) == 0);
	p.rtt_delta_us = 5000;
	c[1].rtt_us = 15000;
	assert(ds_select(&p, &s, c) == 0); /* delta equality: keep */
	c[1].rtt_us = 14999;
	assert(ds_select(&p, &s, c) == 1);
	c[1].rtt_us = 0;
	assert(ds_select(&p, &s, c) == 0); /* unknown previous: switch */
	c[1].rtt_us = 19000;
	assert(ds_select(&p, &s, c) == 0);
	p.generation++;
	assert(ds_select(&p, &s, c) == 1); /* stale hysteresis cleared */
	p.rtt_delta_us = 0;
	c[0].rtt_us = 18000;
	assert(ds_select(&p, &s, c) == 0);
	c[0].rtt_us = UINT_MAX;
	c[1].rtt_us = UINT_MAX - 1;
	assert(ds_select(&p, &s, c) == 1);
	p.rtt_delta_us = UINT_MAX;
	c[0].rtt_us = 1;
	assert(ds_select(&p, &s, c) == 1); /* no addition overflow */
	c[1].position = DS_NONE;
	assert(ds_select(&p, &s, c) == 0);
	/* RTT ignores WRR weights, including zero weights. */
	p.weight_a = 0;
	p.weight_b = 100;
	assert(ds_select(&p, &s, c) == 0);
}

static void test_validation_and_mode_switch(void)
{
	struct ds_policy p = policy(DS_MODE_WRR, 70);
	struct ds_state s = {0};
	struct ds_candidate c[2] = {{2, 100}, {5, 1}};
	assert(ds_policy_valid(&p));
	assert(ds_select(&p, &s, c) == 2);
	p.mode = DS_MODE_RTT;
	assert(ds_select(&p, &s, c) == 5);
	assert(s.credit[0] == 0 && s.credit[1] == 0);
	p.enabled = 0;
	assert(ds_select(&p, &s, c) == DS_NONE);
	p.enabled = 2;
	assert(!ds_policy_valid(&p));
	p.enabled = 1;
	p.mode = 9;
	assert(ds_select(&p, &s, c) == DS_NONE);
	p.mode = DS_MODE_WRR;
	p.weight_a = UINT_MAX;
	assert(!ds_policy_valid(&p));
	p.weight_a = 69;
	assert(!ds_policy_valid(&p));
	p.weight_a = 70;
	p.generation = 0;
	assert(!ds_policy_valid(&p));
}

static void test_multiple_subflows_per_access(void)
{
	struct ds_candidate c = {DS_NONE, 0};
	ds_offer(&c, 7, 0);
	assert(c.position == 7);
	ds_offer(&c, 4, 30000);
	assert(c.position == 4);
	ds_offer(&c, 2, 0);
	assert(c.position == 4);
	ds_offer(&c, 6, 10000);
	assert(c.position == 6);
	ds_offer(&c, 1, 10000);
	assert(c.position == 6); /* stable tie */
}

int main(void)
{
	test_all_ratios_and_generations();
	test_failure_recovery();
	test_rtt_and_hysteresis();
	test_validation_and_mode_switch();
	test_multiple_subflows_per_access();
	puts("selection tests passed: all 101 ratios, generations, failure/recovery, RTT/delta, ABI");
	return 0;
}
