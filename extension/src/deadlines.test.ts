/**
 * deadlines 的单测 —— 判据只有一条真正重要:**保底路径独立于 setTimeout 也能到点**。
 *
 * 这是 2026-08-24 真机定案的直接回归:队首卡死 6 分钟、22s 的放弃闸一次没响,
 * 而同期 worker tick 驱动的心跳一拍不落。所以下面每个用例都不去"等" setTimeout,
 * 而是直接喂一个未来时刻给 sweepDeadlines() —— 那正是 worker tick 走的那条路。
 */

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { armDeadline, pendingDeadlines, sweepDeadlines } from './deadlines';

const FAR = 3_600_000; // 一小时:真实 setTimeout 在测试期内绝不可能自己响

test('保底路径:setTimeout 还远没到点,sweepDeadlines 照样把它触发', () => {
	let fired = 0;
	const handle = armDeadline(FAR, () => { fired += 1; });

	assert.equal(sweepDeadlines(Date.now()), 0, '还没到点就不该响');
	assert.equal(fired, 0);

	assert.equal(sweepDeadlines(Date.now() + FAR + 1), 1, '到点必须响');
	assert.equal(fired, 1);

	// 已经响过的不会再进集合,重复清扫不重复触发。
	assert.equal(sweepDeadlines(Date.now() + FAR + 2), 0);
	assert.equal(fired, 1);
	handle.cancel(); // 幂等,不该炸也不该再触发
	assert.equal(fired, 1);
});

test('cancel 之后两条路径都不再触发', () => {
	let fired = 0;
	const handle = armDeadline(FAR, () => { fired += 1; });
	handle.cancel();
	assert.equal(sweepDeadlines(Date.now() + FAR + 1), 0);
	assert.equal(fired, 0);
});

test('快路径正常时也只响一次(setTimeout 与 sweep 不会双响)', async () => {
	let fired = 0;
	armDeadline(1, () => { fired += 1; });
	await new Promise((r) => setTimeout(r, 20));
	assert.equal(fired, 1, 'setTimeout 快路径应已触发');
	assert.equal(sweepDeadlines(Date.now() + 1000), 0, '已触发的不该被 sweep 再触发一次');
	assert.equal(fired, 1);
});

test('一个守卫抛错不拖垮整轮清扫', () => {
	const order: string[] = [];
	armDeadline(-1, () => { order.push('a'); throw new Error('boom'); });
	armDeadline(-1, () => { order.push('b'); });
	assert.equal(sweepDeadlines(Date.now()), 2);
	assert.deepEqual(order.sort(), ['a', 'b']);
});

test('登记数随 cancel/触发回落,不泄漏', () => {
	const before = pendingDeadlines();
	const h1 = armDeadline(FAR, () => undefined);
	const h2 = armDeadline(FAR, () => undefined);
	assert.equal(pendingDeadlines(), before + 2);
	h1.cancel();
	h2.cancel();
	assert.equal(pendingDeadlines(), before);
});
