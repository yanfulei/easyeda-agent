/**
 * deadlines.ts — 「到点必须发生」的守卫登记处。
 *
 * ── 为什么不能只用 setTimeout(实测,不是推测) ─────────────────────────
 *
 * `transport.ts` 的看门狗早就写明了这件事:
 *
 *   > It runs in a Web Worker because a worker's timer keeps firing while the
 *   > EasyEDA window is backgrounded, whereas the main thread's setInterval is
 *   > throttled/frozen.
 *
 * 但**队列的放弃闸(action-queue)和每次平台调用的 withTimeout(actions.ts)
 * 当时留在了被节流的那条路上**。2026-08-24 的真机日志把后果钉死了:
 *
 *   09:10:47  schematic.power.connect_pin 入队 → 队首
 *   09:10:47 ~ 09:16:52  FIFO 一动不动(6 分钟)
 *              · 心跳照发,而且**一拍不落**(退化前后都是 3.00s/次,worker tick 驱动)
 *              · 旁路 document.current 每次 12ms 就回(主线程活着、eda.* 桥活着、WS 活着)
 *              · 排在后面的 12 条 schematic.pages.list 全部在 daemon 的 18s 预算上超时
 *   09:16:52  队首终于 settle,积压的 12 条**按 FIFO 顺序一次性冲出来**,全部 ok:true
 *
 * 队首那条的回执错误码是 `EDA_*`,**不是 `ACTION_ABANDONED`** —— 也就是说
 * 22s 的放弃闸在 6 分钟里一次都没响(全部三份 daemon 日志里 `ACTION_ABANDONED`
 * 出现 0 次,自 FIFO 上线以来审计日志里也是 0 次)。同一时刻 worker tick 驱动的
 * 心跳一拍不落。**结论只能是:主线程 setTimeout 在后台窗口里被拉长/冻结,而
 * worker postMessage 驱动的回调不受影响。**
 *
 * ── 这个模块做什么 ────────────────────────────────────────────────────
 *
 * 把截止时间登记成**绝对时刻**,两条路径都能触发它,先到者赢:
 *
 *   1. 快路径 —— 仍然挂一个 `setTimeout`(不被节流时它最准);
 *   2. 保底路径 —— `sweepDeadlines()`,由 `transport.ts` 的 **worker tick** 每拍调用。
 *
 * 于是「守卫到点必响」不再依赖主线程定时器的心情。保底路径的分辨率是一个 tick
 * (HEARTBEAT_INTERVAL_MS,3s),所以短于一拍的截止时间在最坏情况下会晚一拍才响
 * —— 这远好过晚六分钟。
 *
 * **不要**把这里当通用定时器用:它只服务「超时/放弃」这类到点必须发生的守卫,
 * 每一个都必须是幂等的(fire 只会被调用一次,由本模块保证)。
 */

/** 一个已登记的截止时间。cancel 之后 fire 永不再被调用。 */
export interface DeadlineHandle {
	/** 取消登记(动作正常 settle 时调用)。幂等。 */
	cancel(): void;
}

interface DeadlineEntry {
	/** 绝对到期时刻(Date.now() 同一时基)。 */
	at: number;
	fire: () => void;
	done: boolean;
	timer: ReturnType<typeof setTimeout> | undefined;
}

const entries = new Set<DeadlineEntry>();

function settle(entry: DeadlineEntry): void {
	if (entry.done) {
		return;
	}
	entry.done = true;
	entries.delete(entry);
	if (entry.timer !== undefined) {
		clearTimeout(entry.timer);
		entry.timer = undefined;
	}
}

/**
 * 登记一个截止时间。到点时 `fire` **恰好被调用一次**(快路径或保底路径,先到者赢);
 * 调用 `cancel()` 之后永不调用。
 *
 * @param delayMs - 从现在起多少毫秒到期(<=0 视为立刻到期,由下一次触发承担)
 * @param fire - 到期回调,必须自身无副作用歧义(只会被调用一次)
 * @returns 取消句柄
 */
export function armDeadline(delayMs: number, fire: () => void): DeadlineHandle {
	const entry: DeadlineEntry = {
		at: Date.now() + delayMs,
		fire,
		done: false,
		timer: undefined,
	};
	entries.add(entry);
	const trip = (): void => {
		if (entry.done) {
			return;
		}
		settle(entry);
		entry.fire();
	};
	try {
		entry.timer = setTimeout(trip, Math.max(0, delayMs));
	}
	catch {
		// 没有 setTimeout 的宿主(理论上不存在)—— 保底路径照样能把它扫出来。
	}
	return {
		cancel(): void {
			settle(entry);
		},
	};
}

/**
 * 保底路径:触发所有已到期的截止时间。由 `transport.ts` 的 worker tick 每拍调用
 * ——**那是本进程里唯一被证明不受后台节流影响的时基**。
 *
 * @param nowMs - 当前时刻(默认 Date.now();测试可注入)
 * @returns 本次触发了几个
 */
export function sweepDeadlines(nowMs: number = Date.now()): number {
	// 先快照:fire 里可能又登记新的截止时间,直接遍历活集合会自食其尾。
	const due: DeadlineEntry[] = [];
	for (const entry of entries) {
		if (!entry.done && entry.at <= nowMs) {
			due.push(entry);
		}
	}
	for (const entry of due) {
		if (entry.done) {
			continue;
		}
		settle(entry);
		try {
			entry.fire();
		}
		catch { /* 一个守卫抛错绝不能拖垮整轮清扫 */ }
	}
	return due.length;
}

/** 当前还挂着的截止时间数量(诊断/测试用)。 */
export function pendingDeadlines(): number {
	return entries.size;
}
