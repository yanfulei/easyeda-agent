package app

// sch_converge_ledger_test.go — 跨调用次数上限的自带测试(#181 第三份复盘)。
//
// 这道门的价值全在「不误伤」上:它要拦住原地打转的第 N 次,同时**永远不能**拦住
// 一个真的在往前推的人。所以下面四条是它的全部合同:
//   ① 同一签名累加,到上限停手;
//   ② 签名一变(有进展)立刻清零;
//   ③ 成功销账 —— 历史失败绝不许拦住未来的正常调用;
//   ④ 台账坏了/关了 = 当作没有,绝不挡活也绝不在用户机器上写文件。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// isolateConvergeLedger 把台账目录钉到临时目录 —— 单测绝不碰真实 HOME。
func isolateConvergeLedger(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(workflow.EnvDir, dir)
	return dir
}

func TestConvergeLedger_CountsSameSignatureAndStops(t *testing.T) {
	isolateConvergeLedger(t)
	var errBuf bytes.Buffer
	key := schConvergeKey{Op: "block-apply", Page: "doc1", Target: "block.wroom"}
	const max = 3

	for i := 1; i <= max; i++ {
		if stop := schConvergeGate("p", key, max); stop != nil && i < max {
			t.Fatalf("第 %d 次就被拦了(上限 %d)—— 门比合同严", i, max)
		}
		n := schConvergeNoteFailure("p", key, "page-too-small:500x820", nil, "拆页", max, &errBuf)
		if n != i {
			t.Fatalf("第 %d 次记账得到 attempts=%d", i, n)
		}
	}
	stop := schConvergeGate("p", key, max)
	if stop == nil {
		t.Fatal("连续 3 次同一签名之后没有停手")
	}
	msg := stop.Error()
	for _, want := range []string{"停手", "不会有别的结果", "画布未改动", "max-attempts", "拆页"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("停手消息少了 %q:\n%s", want, msg)
		}
	}
	// 撞上限之前用户就该看见计数在涨,而不是第 3 次突然被拦。
	if !strings.Contains(errBuf.String(), "2/3") {
		t.Fatalf("stderr 没有逐次提示计数:\n%s", errBuf.String())
	}
}

func TestConvergeLedger_SignatureChangeResets(t *testing.T) {
	isolateConvergeLedger(t)
	key := schConvergeKey{Op: "block-apply", Page: "doc1", Target: "b"}
	const max = 3
	schConvergeNoteFailure("p", key, "overlap:3", nil, "", max, nil)
	schConvergeNoteFailure("p", key, "overlap:3", nil, "", max, nil)
	// 重叠 3→1:这是真进展,计数必须从头起算,否则一个正在收敛的人会被自己的
	// 历史拦住 —— 那比不加上限更糟。
	if n := schConvergeNoteFailure("p", key, "overlap:1", nil, "", max, nil); n != 1 {
		t.Fatalf("签名变化后 attempts=%d,want 1", n)
	}
	if stop := schConvergeGate("p", key, max); stop != nil {
		t.Fatalf("有进展却被停手:%v", stop)
	}
}

func TestConvergeLedger_SuccessClears(t *testing.T) {
	isolateConvergeLedger(t)
	key := schConvergeKey{Op: "destagger", Page: "doc1"}
	const max = 2
	schConvergeNoteFailure("p", key, "marker-overlap:4,moved:0", nil, "", max, nil)
	schConvergeNoteFailure("p", key, "marker-overlap:4,moved:0", nil, "", max, nil)
	if schConvergeGate("p", key, max) == nil {
		t.Fatal("到上限却没停手")
	}
	schConvergeNoteSuccess("p", key)
	if stop := schConvergeGate("p", key, max); stop != nil {
		t.Fatalf("销账之后仍被历史拦住:%v", stop)
	}
}

func TestConvergeLedger_ScopedByPageAndOp(t *testing.T) {
	isolateConvergeLedger(t)
	const max = 2
	p1 := schConvergeKey{Op: "block-apply", Page: "doc1", Target: "b"}
	p2 := schConvergeKey{Op: "block-apply", Page: "doc2", Target: "b"} // 同一个块,另一页
	other := schConvergeKey{Op: "group-move", Page: "doc1", Target: "b"}
	for i := 0; i < max; i++ {
		schConvergeNoteFailure("p", p1, "same", nil, "", max, nil)
	}
	if schConvergeGate("p", p1, max) == nil {
		t.Fatal("p1 到上限却没停手")
	}
	// 「独立成页」正是这道门给出的出路 —— 换一页必须能重新开始,否则出路走不通。
	if stop := schConvergeGate("p", p2, max); stop != nil {
		t.Fatalf("换页之后仍被拦(出路被自己堵死):%v", stop)
	}
	if stop := schConvergeGate("p", other, max); stop != nil {
		t.Fatalf("另一条命令被别人的账拦住:%v", stop)
	}
}

func TestConvergeLedger_DisabledWritesNothing(t *testing.T) {
	dir := isolateConvergeLedger(t)
	key := schConvergeKey{Op: "block-apply", Page: "doc1", Target: "b"}
	if n := schConvergeNoteFailure("p", key, "sig", nil, "", 0, nil); n != 0 {
		t.Fatalf("上限关掉时仍在记账(attempts=%d)", n)
	}
	if _, err := os.Stat(schConvergeLedgerPath("p")); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("上限关掉却写了台账文件(目录内容 %v)", entries)
	}
	if stop := schConvergeGate("p", key, 0); stop != nil {
		t.Fatalf("上限为 0 应当完全不拦:%v", stop)
	}
}

func TestConvergeLedger_CorruptFileNeverBlocks(t *testing.T) {
	dir := isolateConvergeLedger(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(schConvergeLedgerPath("p"), []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := schConvergeKey{Op: "block-apply", Page: "doc1", Target: "b"}
	// 布局工具的可用性不该依赖一个缓存文件:坏台账 = 空台账,照常放行、照常能记新账。
	if stop := schConvergeGate("p", key, 1); stop != nil {
		t.Fatalf("坏台账把命令挡住了:%v", stop)
	}
	if n := schConvergeNoteFailure("p", key, "sig", nil, "", 3, nil); n != 1 {
		t.Fatalf("坏台账之后记不了新账(attempts=%d)", n)
	}
}

func TestConvergeLedger_FitCarriesToNextRun(t *testing.T) {
	isolateConvergeLedger(t)
	key := schConvergeKey{Op: "block-apply", Page: "doc1", Target: "block.wroom"}
	fit := judgeSchPageFit("wroom", boxAt(50, 20, 300, 820), a4Usable(), a4Keepout())
	if !fit.TooBig() {
		t.Fatalf("样本没构造成 page-too-small(verdict=%s)", fit.Verdict)
	}
	schConvergeNoteFailure("p", key, "page-too-small:300x820", &fit, fit.Advice, 3, nil)

	// 「落块前用实测 bbox 判断」在"没落地就没实测"这个死结下唯一诚实的解法:
	// 用上一轮的实测。这条读路必须跨进程可用(台账已落盘)。
	got := schConvergeFitFor("p", key)
	if got == nil || !got.TooBig() {
		t.Fatalf("上一轮的实测判决没带过来:%+v", got)
	}
	if got.H != fit.H || !got.Measured {
		t.Fatalf("带过来的实测数据不对:%+v", got)
	}
}

func TestConvergeLedger_PathUnderWorkflowDir(t *testing.T) {
	dir := isolateConvergeLedger(t)
	// 与 workflow 状态同目录但**是独立文件**:台账是可丢弃的诊断数据,写坏了不该
	// 拖垮带阶段门/指纹校验的工作流状态。
	got := schConvergeLedgerPath("ce shi/x")
	if filepath.Dir(got) != dir {
		t.Fatalf("台账不在 workflow 目录下:%s", got)
	}
	if got == workflow.Path("ce shi/x") {
		t.Fatal("台账与 workflow 状态共用同一个文件 —— 必须分开")
	}
	if strings.ContainsAny(filepath.Base(got), `/\ `) {
		t.Fatalf("工程名没被清洗成安全文件名:%s", filepath.Base(got))
	}
}
