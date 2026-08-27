package app

import (
	"bytes"
	"strings"
	"testing"
)

// 回归:连接器自己的旋转探测旗留在画布上,被算成一条 orphan-flag,
// `sch gate --strict`(S5 逐页门用的就是它)当场 FAIL —— 一块电路完全正确的板子
// 过不了自己的门。
//
// 2026-08-26 esp32MiniRequire 实测(POWER 页):
//
//	sch bridge-check: 1 problem tree(s) — 0 wire-bridge, 0 orphan-stub,
//	  1 orphan-flag(flag on no wire), 0 orphan-tree
//	  WARN orphan-flag ORPHAN_FLAG nets=[__ROTPROBE__] flags: 2693f1f2f947e107
//	sch gate: FAIL — bridge-check: 1 orphan-flag (--strict)
//
// 来源:extension/src/actions.ts 的 detectRotationNegation() 造一支
// `__ROTPROBE__` 探测旗读回 rotation 再 delete —— 而那句 delete 没有回读验证,
// 「删除撒谎」是平台已知病。
func TestParseBridgeReport_ToolProbeResidueIsNotABoardProblem(t *testing.T) {
	result := map[string]any{
		"passed": false,
		"summary": map[string]any{
			"trees": 2, "bridges": 0, "orphans": 0,
			"orphanFlags": 1, "orphanTrees": 1, "wireTreesTotal": 16,
		},
		"trees": []any{
			// 工具残留
			map[string]any{"kind": "ORPHAN_FLAG", "nets": []any{"__ROTPROBE__"},
				"flagIds": []any{"2693f1f2f947e107"}},
			// 真问题:挪件留下的悬空树
			map[string]any{"kind": "ORPHAN_TREE", "nets": []any{"GND"},
				"wireIds": []any{"w1"}, "flagIds": []any{"f1"}},
		},
	}
	rep, err := parseBridgeReport(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ToolProbes) != 1 {
		t.Fatalf("探测残留没被摘出来:%+v", rep.ToolProbes)
	}
	if len(rep.Trees) != 1 || rep.Trees[0].Kind != "ORPHAN_TREE" {
		t.Fatalf("摘错了 —— 真问题必须留下:%+v", rep.Trees)
	}
	// 计数必须与 trees 是同一份事实,否则报头说 1 orphan-flag、明细里却没有。
	if rep.Summary.OrphanFlags != 0 {
		t.Fatalf("orphanFlags 没重算:%d", rep.Summary.OrphanFlags)
	}
	if rep.Summary.OrphanTrees != 1 || rep.Summary.Trees != 1 {
		t.Fatalf("真问题的计数被误伤:%+v", rep.Summary)
	}
	// WireTreesTotal 是画布事实(探测旗确实占着一棵树),不该被改。
	if rep.Summary.WireTreesTotal != 16 {
		t.Fatalf("WireTreesTotal 是画布事实,不该改:%d", rep.Summary.WireTreesTotal)
	}

	// 报告里必须**说出来**并给清理命令 —— 静默忽略等于让垃圾长住画布。
	var buf bytes.Buffer
	renderBridgeReport(rep, &buf)
	out := buf.String()
	for _, kw := range []string{"tool-probe-residue", "prim-delete", "2693f1f2f947e107"} {
		if !strings.Contains(out, kw) {
			t.Fatalf("报告没写清 %q:\n%s", kw, out)
		}
	}
}

// 只有真短路才该让 passed=false —— 摘掉残留后若只剩 WARN,passed 由 Bridges 决定。
func TestParseBridgeReport_OnlyProbeResidueLeavesNoBoardProblem(t *testing.T) {
	result := map[string]any{
		"passed": false,
		"summary": map[string]any{
			"trees": 1, "bridges": 0, "orphans": 0,
			"orphanFlags": 1, "orphanTrees": 0, "wireTreesTotal": 16,
		},
		"trees": []any{
			map[string]any{"kind": "ORPHAN_FLAG", "nets": []any{"__ROTPROBE__"},
				"flagIds": []any{"probe1"}},
		},
	}
	rep, err := parseBridgeReport(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Trees) != 0 || rep.Summary.Trees != 0 {
		t.Fatalf("摘完应当一个问题树都不剩:%+v", rep.Trees)
	}
	if !rep.Passed {
		t.Fatal("只剩工具残留时,板子本身没有问题 —— passed 该为 true")
	}
}

// 判据是**精确网名**:用户自己的 PROBE_* 网绝不能被误摘。
func TestIsSchToolProbeNet_ExactMatchOnly(t *testing.T) {
	if !isSchToolProbeNet("__ROTPROBE__") {
		t.Fatal("认不出连接器的探测网名")
	}
	for _, n := range []string{"PROBE_5V", "__ROTPROBE__X", "ROTPROBE", "GND", ""} {
		if isSchToolProbeNet(n) {
			t.Fatalf("%q 被误判成工具残留 —— 前缀/包含匹配会摘掉用户自己的网", n)
		}
	}
}

// 挂着真网名的树**不算**残留:新线穿过会继承那个真网名,它已经和电路有牵连。
func TestSchTreeIsToolProbe_MixedNetsStayReal(t *testing.T) {
	if schTreeIsToolProbe(bridgeTree{Kind: "ORPHAN_FLAG", Nets: []string{"__ROTPROBE__", "GND"}}) {
		t.Fatal("混着真网名的树被当成纯工具残留摘掉了")
	}
	if schTreeIsToolProbe(bridgeTree{Kind: "ORPHAN_FLAG"}) {
		t.Fatal("没有网名的树不该被当成工具残留")
	}
}
