package app

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// 回归:恢复段被「连接器队列暂时阻塞」当场判死。
//
// 2026-08-26 esp32MiniRequire 实测,`sch zone move` 的完整输出:
//
//	warn: zone-move 移动失败 —— 立即按快照对全页 4 个引脚重连(含第三方偏离 pin)
//	zone move 刚移失败:…;恢复重连本身失败(--doc guard: schematic.pages.list failed:
//	  the connector's action queue is blocked by schematic.power.connect_pin (req_4509) for 4s),
//	  以下引脚待手工恢复:SW1:2→GND SW2:2→GND SW2:1→EN SW1:1→IO0
//
// 火警现场的恢复动作,被**刚才那个失败本身**造成的队列阻塞挡在门外 ——
// 恢复段每一步都要过 --doc guard(一次 pages.list),而队列正被那条 connect_pin 堵着。
//
// 而 daemon 的拒绝回执自己就写着「Next step: **wait** — the daemon re-checks by
// itself and this refusal stops the moment the queue drains」,并且明确
// "this action was NOT sent" —— 动作根本没发出去,等队列排空再发既安全又是官方建议。
// CLI 侧却没等。
func TestIsQueueBlocked_MatchesByCodeNotText(t *testing.T) {
	blocked := &actionError{Action: "schematic.pages.list", Code: connectorQueueBlockedCode,
		Message: "the connector's action queue is blocked by schematic.power.connect_pin (req_4509) for 4s — this action was NOT sent"}
	if !isQueueBlocked(blocked) {
		t.Fatal("按错误码认不出队列阻塞")
	}
	// 判据必须是错误码:文本会随措辞漂移,而这条路径决定「等还是放弃」。
	textOnly := errors.New("the connector's action queue is blocked by foo for 4s")
	if isQueueBlocked(textOnly) {
		t.Fatal("靠文本匹配认队列阻塞 —— 措辞一改就失灵")
	}
	if isQueueBlocked(&actionError{Code: staleReadCode}) {
		t.Fatal("把 STALE_READ 误判成队列阻塞:两者下一步完全不同")
	}
	if isQueueBlocked(nil) {
		t.Fatal("nil 不该被判成队列阻塞")
	}
	// 包一层也要认得出来 —— 恢复段的错误都是层层包上来的。
	if !isQueueBlocked(fmt.Errorf("--doc guard: %w", blocked)) {
		t.Fatal("包装过的队列阻塞错误认不出来")
	}
}

// 队列排空后必须自己接着跑完,而不是把这次调用判死。
func TestRetryWhileQueueBlocked_WaitsThenSucceeds(t *testing.T) {
	calls := 0
	var slept []time.Duration
	err := retryWhileQueueBlocked("测试动作", func() error {
		calls++
		if calls < 3 {
			return &actionError{Action: "schematic.pages.list", Code: connectorQueueBlockedCode,
				Message: "blocked by connect_pin for 4s"}
		}
		return nil
	}, queueBlockRetryPolicy{Budget: time.Minute, Step: 10 * time.Millisecond,
		sleep: func(d time.Duration) { slept = append(slept, d) }})
	if err != nil {
		t.Fatalf("队列排空后应当成功,却返回:%v", err)
	}
	if calls != 3 {
		t.Fatalf("重试次数=%d,应当一直等到队列排空", calls)
	}
	if len(slept) != 2 {
		t.Fatalf("每次阻塞之间都该退避一次,实际 %d 次", len(slept))
	}
	// 退避必须递增 —— 定长轮询在长 handler 上就是无效空转。
	if slept[1] <= slept[0] {
		t.Fatalf("退避没有递增:%v", slept)
	}
}

// 非队列阻塞的错误必须**立刻**原样抛出:重试一个真失败等于把故障拖长。
func TestRetryWhileQueueBlocked_OtherErrorsFailFast(t *testing.T) {
	calls := 0
	boom := errors.New("真失败")
	err := retryWhileQueueBlocked("测试动作", func() error {
		calls++
		return boom
	}, queueBlockRetryPolicy{Budget: time.Minute, Step: time.Millisecond,
		sleep: func(time.Duration) { t.Fatal("非队列阻塞不该退避") }})
	if !errors.Is(err, boom) {
		t.Fatalf("真失败应原样抛出,实际:%v", err)
	}
	if calls != 1 {
		t.Fatalf("真失败被重试了 %d 次", calls)
	}
}

// 永远排不空时必须有底线:烧完预算就把**原始拒绝**抛出去(它自带下一步指引),
// 绝不无限等 —— 那会把「命令失败」变成「命令挂死」。
func TestRetryWhileQueueBlocked_GivesUpAfterBudget(t *testing.T) {
	calls := 0
	var total time.Duration
	blocked := &actionError{Action: "schematic.pages.list", Code: connectorQueueBlockedCode,
		Message: "blocked by connect_pin for 99s"}
	err := retryWhileQueueBlocked("测试动作", func() error {
		calls++
		return blocked
	}, queueBlockRetryPolicy{Budget: 100 * time.Millisecond, Step: 30 * time.Millisecond,
		sleep: func(d time.Duration) { total += d }})
	if !isQueueBlocked(err) {
		t.Fatalf("超预算后应抛出原始队列阻塞错误(自带下一步),实际:%v", err)
	}
	if total > 200*time.Millisecond {
		t.Fatalf("等待远超预算:%v", total)
	}
	if calls < 2 {
		t.Fatalf("预算内至少该重试一次,实际只调用 %d 次", calls)
	}
}
