package app

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// ── 连接器队列暂时阻塞 → 等,不要判死 ────────────────────────────────────────
//
// daemon 在窗口 FIFO 被上一条 handler 堵住时会**拒绝派发**并回 CONNECTOR_QUEUE_BLOCKED。
// 那条回执自己就写明了两件事:
//
//	① "this action was NOT sent"      —— 动作根本没发出去,重发不会造成重复写;
//	② "Next step: wait — the daemon re-checks by itself and this refusal stops
//	   the moment the queue drains"   —— 官方建议就是等。
//
// CLI 侧却把它当普通失败原样上抛,于是 2026-08-26 端到端里出现了最坏的一幕:
// **恢复段被它自己要修的那个故障挡在门外** —— zone-move / group-move 的重连失败
// 触发恢复段,恢复段每一步要过 --doc guard(一次 pages.list),而队列正被刚失败的
// connect_pin 堵着,于是恢复重连整个跑不起来,U2 的七条连接就此丢在画布上。
//
// 这里给所有「失败代价高、且必须跑完」的动作一条统一的等待通道。判据是**错误码**
// 不是文本(措辞会漂),预算内退避重试,烧完预算把**原始拒绝**抛回去(它自带下一步)。

// connectorQueueBlockedCode 与 internal/daemon/queueblock.go 的回执码一致。
const connectorQueueBlockedCode = "CONNECTOR_QUEUE_BLOCKED"

// errQueueBlocked 是 errors.Is 的哨兵(与 errStaleRead 同一套路数)。
var errQueueBlocked = errors.New("connector action queue blocked")

// isQueueBlocked 报告 err 是不是「队列暂时堵着,动作没发出去」。
// 判据是 actionError.Code —— 与 isStaleRead 同口径,绝不做文本匹配。
func isQueueBlocked(err error) bool {
	if err == nil {
		return false
	}
	var ae *actionError
	if errors.As(err, &ae) {
		return ae.Code == connectorQueueBlockedCode
	}
	return false
}

// 默认预算:实测队列被一条 connect_pin 堵住时,12~15s 内基本都能排空
// (2026-08-26 端到端里 6 次阻塞,每次首轮 12s 轮询即恢复)。给到 90s 留足余量,
// 又不至于把「永远排不空」拖成挂死。
const (
	queueBlockRetryBudget = 90 * time.Second
	queueBlockRetryStep   = 3 * time.Second
)

// queueBlockRetryPolicy 是等待策略(sleep 可注入,测试用)。
type queueBlockRetryPolicy struct {
	Budget time.Duration
	Step   time.Duration
	// Stderr 非空时,每轮等待打一行进度 —— 静默地等 90 秒比失败更让人困惑。
	Stderr io.Writer
	sleep  func(time.Duration)
}

func (p queueBlockRetryPolicy) napper() func(time.Duration) {
	if p.sleep != nil {
		return p.sleep
	}
	return time.Sleep
}

// retryWhileQueueBlocked 跑 fn;只要它因**队列阻塞**失败,就退避后重试,直到成功、
// 变成别的错误、或烧完预算。
//
// 三条纪律:
//   - 只对 CONNECTOR_QUEUE_BLOCKED 重试 —— 真失败必须立刻原样抛出,重试真失败
//     等于把故障拖长;
//   - 退避递增 —— 定长轮询碰上长 handler 就是无效空转;
//   - 烧完预算抛**原始**拒绝 —— 它自带「切前台/重启 EasyEDA」的下一步,
//     包一层自造措辞只会把可执行的指引冲淡。
func retryWhileQueueBlocked(label string, fn func() error, pol queueBlockRetryPolicy) error {
	if pol.Budget <= 0 {
		pol.Budget = queueBlockRetryBudget
	}
	if pol.Step <= 0 {
		pol.Step = queueBlockRetryStep
	}
	nap := pol.napper()
	var spent time.Duration
	round := 0
	for {
		err := fn()
		if !isQueueBlocked(err) {
			return err // 成功,或者是别的错误 —— 两种都立刻返回
		}
		round++
		wait := time.Duration(round) * pol.Step // 3s, 6s, 9s …
		if spent+wait > pol.Budget {
			return err // 预算烧完:把原始拒绝抛回去(自带下一步)
		}
		if pol.Stderr != nil {
			fmt.Fprintf(pol.Stderr,
				"  ⏳ %s:连接器队列被上一条动作堵着(动作未发出),等 %s 后重试(已等 %s / 预算 %s)\n",
				label, wait, spent.Round(time.Second), pol.Budget)
		}
		nap(wait)
		spent += wait
	}
}
