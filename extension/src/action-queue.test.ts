/**
 * action-queue 的单测 —— 判据全部机械可验,不靠"看起来对了"。
 *
 * 覆盖六条硬性质(缺任何一条,读写对齐就退回启发式):
 *   1. FIFO 保序:提交顺序 === 执行顺序 === 完成顺序,且 seq 逐个 +1。
 *   2. 超时放弃后队列继续流动,seqAbandoned 递增并点名 request id。
 *   3. 截止时间来自**请求自带的 timeoutMs**,不是写死常数(长操作不被误杀)。
 *   4. 排队已经耗尽 absolute deadline 的任务**不得执行**。
 *   5. 被放弃的写进入 quarantine,原 handler settle 前后续写不得执行。
 *   6. 旁路响应打 unordered 且不动 seq;队列溢出被明确拒绝且**没有执行**。
 */

import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
	ABANDONED_ID_RING,
	ActionQueue,
	isBypassAction,
	type QueueOutcome,
} from './action-queue';
import { sweepDeadlines } from './deadlines';

interface Deferred<T> {
	promise: Promise<T>;
	resolve: (value: T) => void;
	reject: (err: unknown) => void;
}

function deferred<T>(): Deferred<T> {
	let resolve!: (value: T) => void;
	let reject!: (err: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

/** 测试用队列:grace=0,好让 timeoutMs 就是真实截止时间。 */
function testQueue(maxDepth = 64): ActionQueue {
	return new ActionQueue({ maxDepth, graceMs: 0, fallbackTimeoutMs: 1000 });
}

test('FIFO:提交顺序就是执行顺序,后来的绝不抢在前面的 settle 之前开跑', async () => {
	const q = testQueue();
	const events: string[] = [];

	// 故意让先提交的最慢 —— 并发实现下它一定被后面的超过(探针实测过这个形态)。
	const holds = [60, 20, 0];
	const outcomes = holds.map((ms, i) => q.submit({
		id: `req-${i}`,
		timeoutMs: 5000,
		run: async () => {
			events.push(`enter-${i}`);
			await sleep(ms);
			events.push(`exit-${i}`);
			return i;
		},
	}));

	const settled = await Promise.all(outcomes);

	assert.deepEqual(events, [
		'enter-0', 'exit-0',
		'enter-1', 'exit-1',
		'enter-2', 'exit-2',
	], '任何交错都意味着 happens-before 不成立');

	for (const [i, outcome] of settled.entries()) {
		assert.equal(outcome.status, 'ok');
		// seq 是「本动作完成之后」的值 → 第 i 个完成的动作带 i+1。
		assert.equal(outcome.stamp.seq, i + 1, `第 ${i} 个动作的 seq 不对`);
		assert.equal(outcome.stamp.seqAbandoned, 0);
		assert.equal(outcome.stamp.unordered, undefined, 'FIFO 响应不得打 unordered');
	}
});

test('settle 包括 reject:失败的 handler 照样算「完成」,seq 不留空洞', async () => {
	const q = testQueue();
	const bad = await q.submit({ id: 'bad', timeoutMs: 5000, run: async () => { throw new Error('boom'); } });
	const good = await q.submit({ id: 'good', timeoutMs: 5000, run: async () => 1 });

	assert.equal(bad.status, 'error');
	assert.equal(bad.stamp.seq, 1);
	assert.equal(good.stamp.seq, 2, '一次失败绝不能把链断掉或让 seq 跳号');
	assert.equal(good.stamp.seqAbandoned, 0, '失败 ≠ 放弃:seqAbandoned 不得因为 reject 而动');
});

test('放弃:队首永不 resolve 时,队列继续流动且 seqAbandoned 递增并点名', async () => {
	const q = testQueue();
	const stuck = deferred<number>();
	let secondRan = false;

	const first = q.submit({ id: 'req-wedged', timeoutMs: 30, run: () => stuck.promise });
	const second = q.submit({
		id: 'req-after',
		timeoutMs: 5000,
		run: async () => { secondRan = true; return 2; },
	});

	const firstOutcome = await first;
	assert.equal(firstOutcome.status, 'abandoned', '卡死的队首必须被放弃,否则它吞掉后面的一切');
	assert.equal(firstOutcome.stamp.seq, 0, '被放弃的动作没有可定位的完成时刻,绝不给它补 seq');
	assert.equal(firstOutcome.stamp.seqAbandoned, 1);
	assert.deepEqual(firstOutcome.stamp.abandonedIds, ['req-wedged'], '判定要能点名,不能只数数');

	const secondOutcome = await second;
	assert.equal(secondOutcome.status, 'ok', '放弃之后队列必须继续流动');
	assert.equal(secondRan, true);
	assert.equal(secondOutcome.stamp.seq, 1);
	assert.equal(secondOutcome.stamp.seqAbandoned, 1,
		'后续响应必须继续带着递增后的 seqAbandoned —— Go 侧正是靠它作废那段时间的结论');
	assert.deepEqual(secondOutcome.stamp.abandonedIds, ['req-wedged']);

	stuck.resolve(0); // 收尾:被放弃的 handler 稍后 settle,不得再动 seq
	await sleep(20);
	const third = await q.submit({ id: 'req-third', timeoutMs: 5000, run: async () => 3 });
	assert.equal(third.stamp.seq, 2, '被放弃的动作事后 settle 不得回头补计数');
});

test('截止时间用请求自带的 timeoutMs —— 长操作不被误杀,短操作照样兜住', async () => {
	const q = testQueue();

	// 合法长操作(sch check / DRC 能跑 60s+):预算给足就必须让它跑完。
	const long = await q.submit({
		id: 'long',
		timeoutMs: 400,
		run: async () => { await sleep(120); return 'done'; },
	});
	assert.equal(long.status, 'ok', '预算内的长操作被放弃 = 写死常数的误杀');

	// 同一条队列上预算很小的那个照样按自己的预算被放弃。
	const short = await q.submit({
		id: 'short',
		timeoutMs: 20,
		run: async () => { await sleep(300); return 'late'; },
	});
	assert.equal(short.status, 'abandoned');
	assert.equal(short.stamp.seqAbandoned, 1);
});

test('absolute deadline 包含排队时间:轮到时已过期的动作根本不执行', async () => {
	const q = testQueue();
	const head = deferred<number>();
	const createdAtMs = Date.now();
	const first = q.submit({ id: 'head', timeoutMs: 3_600_000, createdAtMs, run: () => head.promise });

	let expiredRan = false;
	const expired = q.submit({
		id: 'expired-in-queue',
		timeoutMs: 20,
		createdAtMs: createdAtMs - 1_000,
		run: async () => { expiredRan = true; return 2; },
	});

	head.resolve(1);
	assert.equal((await first).status, 'ok');
	const outcome = await expired;
	assert.equal(outcome.status, 'expired');
	assert.equal(expiredRan, false, 'deadline 已耗尽的晚到写/读都不得再碰编辑器');
	assert.equal(outcome.stamp.seq, 1, '未执行的过期任务不得推进 handler 完成序号');
});

test('mutating abandon 进入 quarantine:只读可诊断,后续写等原 handler settle 后才恢复', async () => {
	const q = testQueue();
	const stuck = deferred<number>();
	const abandoned = q.submit({
		id: 'ghost-write',
		action: 'schematic.component.place',
		mutates: true,
		timeoutMs: 3_600_000,
		run: () => stuck.promise,
	});
	await Promise.resolve(); // runHead 已登记 deadline;不用墙钟制造竞态
	sweepDeadlines(Date.now() + 7_200_000);

	let queuedWriteRan = false;
	const queuedWrite = q.submit({
		id: 'queued-write',
		action: 'schematic.wire.create',
		mutates: true,
		timeoutMs: 500,
		run: async () => { queuedWriteRan = true; return 2; },
	});
	const read = q.submit({ id: 'diagnostic-read', action: 'schematic.components.list', timeoutMs: 500, run: async () => 3 });

	const firstOutcome = await abandoned;
	assert.equal(firstOutcome.status, 'abandoned');
	if (firstOutcome.status === 'abandoned') assert.equal(firstOutcome.mutates, true);
	const blockedQueued = await queuedWrite;
	assert.equal(blockedQueued.status, 'quarantined');
	assert.equal(queuedWriteRan, false, 'abandoned write 未 settle 时绝不能让下一笔写交错进去');
	assert.equal((await read).status, 'ok', 'quarantine 期仍需保留只读诊断面');

	let immediateWriteRan = false;
	const blockedImmediate = await q.submit({
		id: 'autosave',
		action: 'schematic.save',
		mutates: true,
		timeoutMs: 500,
		run: async () => { immediateWriteRan = true; return 4; },
	});
	assert.equal(blockedImmediate.status, 'quarantined');
	assert.equal(immediateWriteRan, false);

	stuck.resolve(1);
	await sleep(0); // late settlement clears this exact quarantine generation
	const recovered = await q.submit({
		id: 'write-after-settle',
		action: 'schematic.save',
		mutates: true,
		timeoutMs: 500,
		run: async () => 5,
	});
	assert.equal(recovered.status, 'ok', '原 handler settle 后写通道必须自动恢复');
});

test('abandoned write 隔离所有前台上下文切换,不让晚到写跨文档落地', async () => {
	for (const action of ['document.open', 'document.close', 'schematic.page.open']) {
		const q = testQueue();
		const stuck = deferred<number>();
		const abandoned = q.submit({
			id: `ghost-before-${action}`,
			action: 'schematic.component.place',
			mutates: true,
			writeSensitive: true,
			timeoutMs: 3_600_000,
			run: () => stuck.promise,
		});
		// Let the FIFO microtask enter runHead and register its deadline before
		// simulating the worker tick. A single Promise microtask is insufficient
		// when the test runner has queued the chain continuation behind it.
		await sleep(0);
		sweepDeadlines(Date.now() + 7_200_000);
		assert.equal((await abandoned).status, 'abandoned');

		let switched = false;
		const blocked = await q.submit({
			id: `switch-${action}`,
			action,
			mutates: false,
			writeSensitive: true,
			timeoutMs: 5_000,
			run: async () => { switched = true; return 1; },
		});
		assert.equal(blocked.status, 'quarantined', `${action} 必须被 abandoned-write quarantine 拦下`);
		assert.equal(switched, false, `${action} 被拒后不能触碰编辑器上下文`);
		stuck.resolve(1);
		await sleep(0);
	}
});

test('放弃闸不依赖 setTimeout —— 只靠 worker tick 的 sweepDeadlines 也必须到点', async () => {
	// 2026-08-24 真机定案的直接回归:后台窗口里主线程 setTimeout 被节流/冻结,
	// 22s 的放弃闸在 6 分钟里一次没响,队首把后面 12 条全堵死。这里把预算设成
	// 一小时 —— 真实 setTimeout 在测试期内绝不可能自己响 —— 然后只喂一个未来
	// 时刻给 sweepDeadlines()(那正是 worker tick 走的路)。它必须照样放弃队首。
	const q = new ActionQueue({ graceMs: 0, fallbackTimeoutMs: 1000 });
	const stuck = deferred<number>();
	let secondRan = false;

	const first = q.submit({ id: 'req-frozen-timers', timeoutMs: 3_600_000, run: () => stuck.promise });
	const second = q.submit({ id: 'req-behind', timeoutMs: 3_600_000, run: async () => { secondRan = true; return 2; } });

	await sleep(5); // 让队首真正开跑并登记好截止时间
	assert.equal(secondRan, false, '前提:队首还在跑,后面那条被 FIFO 挡着');

	sweepDeadlines(Date.now() + 3_600_001);

	const firstOutcome = await first;
	assert.equal(firstOutcome.status, 'abandoned', 'setTimeout 被冻结时,worker tick 就是唯一的闸门');
	assert.equal(firstOutcome.stamp.seqAbandoned, 1);
	assert.deepEqual(firstOutcome.stamp.abandonedIds, ['req-frozen-timers']);

	const secondOutcome = await second;
	assert.equal(secondOutcome.status, 'ok', '放弃之后积压必须继续流动');
	assert.equal(secondRan, true);

	stuck.resolve(0);
});

test('旁路:wedge 期仍可观测,且响应必须打 unordered、不动 seq', async () => {
	const q = testQueue();
	const stuck = deferred<number>();
	const wedged = q.submit({ id: 'wedged', timeoutMs: 200, run: () => stuck.promise });

	// 队首卡死期间提交旁路读 —— 它必须立刻拿到答案(这是 wedge 期唯一的观测手段)。
	const probe = await q.submit({ id: 'probe', bypass: true, run: async () => 'alive' });
	assert.equal(probe.status, 'ok');
	assert.equal(probe.stamp.unordered, true, '旁路响应必须自曝身份,否则会被误当成顺序证据');
	assert.equal(probe.stamp.seq, 0, '旁路不在 FIFO 里,绝不推进 seq');

	const after = await wedged;
	assert.equal(after.status, 'abandoned');
	stuck.resolve(0);
});

test('旁路名单必须短,且只含纯诊断读', () => {
	assert.equal(isBypassAction('document.current'), true);
	for (const mutating of [
		'schematic.component.place', 'schematic.component.delete', 'schematic.wire.create',
		'schematic.components.list', 'pcb.drc.check', 'debug.exec_js',
	]) {
		assert.equal(isBypassAction(mutating), false, `${mutating} 绝不能走旁路`);
	}
});

test('溢出:队列满时明确拒绝,且被拒的动作根本没有执行', async () => {
	const q = testQueue(2);
	const stuck = deferred<number>();

	// 队首开跑后 depth 归零,所以再排 2 个才填满积压。
	const head = q.submit({ id: 'head', timeoutMs: 100, run: () => stuck.promise });
	await sleep(5);
	const q1 = q.submit({ id: 'q1', timeoutMs: 5000, run: async () => 1 });
	const q2 = q.submit({ id: 'q2', timeoutMs: 5000, run: async () => 2 });

	let overflowRan = false;
	const rejected = await q.submit({
		id: 'q3',
		timeoutMs: 5000,
		run: async () => { overflowRan = true; return 3; },
	});

	assert.equal(rejected.status, 'overflow');
	assert.equal(overflowRan, false, '被拒的动作必须没有执行 —— 否则「没执行」这句话是假的');
	if (rejected.status === 'overflow') {
		assert.equal(rejected.depth, 2);
	}

	stuck.resolve(0);
	await Promise.all([head, q1, q2]);
});

test('abandonedIds 是有界环形缓冲,不会无限增长', async () => {
	const q = testQueue();
	let last: QueueOutcome<number> | undefined;
	for (let i = 0; i < ABANDONED_ID_RING + 3; i++) {
		const pending = q.submit({
			id: `a-${i}`,
			timeoutMs: 3_600_000,
			run: () => new Promise<number>(() => { /* 永不 settle */ }),
		});
		await sleep(0);
		sweepDeadlines(Date.now() + 7_200_000);
		last = await pending;
	}
	assert.ok(last);
	assert.equal(last.status, 'abandoned');
	assert.equal(last.stamp.seqAbandoned, ABANDONED_ID_RING + 3, 'seqAbandoned 是累计数,绝不受缓冲长度影响');
	assert.equal(last.stamp.abandonedIds?.length, ABANDONED_ID_RING);
	assert.equal(last.stamp.abandonedIds?.[ABANDONED_ID_RING - 1], `a-${ABANDONED_ID_RING + 2}`, '保留的是最近的');
});
