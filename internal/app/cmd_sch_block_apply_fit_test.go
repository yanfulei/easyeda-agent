package app

// cmd_sch_block_apply_fit_test.go — block-apply 侧「这一页放不下这个块」的自带测试。
//
// 口径的关键在 bapBlockPageFit 取的是**虚拟组包络**(器件 ∪ 它自己的 marker/桩线)
// 而不是器件 bbox:复盘里那些 700–840 高的块,一半的高是竖排 marker 撑出来的,
// 只看器件本体会系统性低估、于是永远报不出"放不下"。

import (
	"strings"
	"testing"
)

func fitCluster(desig string, b layoutBBox) schCluster {
	return schCluster{Designator: desig, Body: b, Box: b}
}

func TestBapBlockPageFit_UnionsOwnClustersOnly(t *testing.T) {
	usable := a4Usable()
	ko := a4Keepout()
	man := &bapManifest{
		BlockID:   "block.esp32s3_wroom1_module",
		GroupName: "esp32s3_wroom1_module(U3)",
		Placed: []bapPlacement{
			{Designator: "U3"}, {Designator: "C11"},
		},
	}
	clusters := []schCluster{
		fitCluster("U3", boxAt(60, 40, 200, 700)),
		fitCluster("C11", boxAt(60, 740, 120, 80)), // 组高 = 40..820 → 780
		// 别人的组不许算进来 —— 算进来就会把"整页很挤"误报成"这个块放不下"。
		fitCluster("R99", boxAt(900, 40, 200, 700)),
	}
	f := bapBlockPageFit(clusters, man, &usable, ko)
	if f == nil {
		t.Fatal("匹配得上却没给判决")
	}
	if f.W != 200 || f.H != 780 {
		t.Fatalf("包络算错:%.0f×%.0f,want 200×780", f.W, f.H)
	}
	if f.Name != "esp32s3_wroom1_module(U3)" {
		t.Fatalf("名字该用组名(它是能直接喂给 group-move 的抓手),got %q", f.Name)
	}
	if !f.Measured {
		t.Fatal("这条路径读的是实测 bbox,不该自称估算")
	}
	// 780 塞不进图签左侧(宽够但…宽 200 < 456 其实塞得下)—— 这里只钉住"有判决",
	// 具体档位由 sch_page_fit_test.go 的三档用例负责。
	if f.Verdict == "" {
		t.Fatal("verdict 为空")
	}
}

func TestBapBlockPageFit_TooBigWhenTallerThanPage(t *testing.T) {
	usable := a4Usable() // 高 801
	ko := a4Keepout()
	man := &bapManifest{BlockID: "block.legacy", Placed: []bapPlacement{{Designator: "U1"}}}
	f := bapBlockPageFit([]schCluster{fitCluster("U1", boxAt(60, 10, 300, 820))}, man, &usable, ko)
	if f == nil || !f.TooBig() {
		t.Fatalf("实测高 820 > 可用高 801 却没判 page-too-small:%+v", f)
	}
	if !strings.Contains(f.Advice, "820") {
		t.Fatalf("建议里没有实测数字:%s", f.Advice)
	}
	if f.Name != "legacy" {
		t.Fatalf("没有组名时该退回块短名,got %q", f.Name)
	}
}

// TestBapBlockPageFit_SilentWhenItCannotKnow 钉住「宁可沉默也不猜」:
// 量不出可用区(页面没有图框)或本实例一个位号都没匹配上时,必须返回 nil。
// 猜出来的 fits 会直接掩盖掉这次要报的那句话。
func TestBapBlockPageFit_SilentWhenItCannotKnow(t *testing.T) {
	usable := a4Usable()
	man := &bapManifest{BlockID: "block.x", Placed: []bapPlacement{{Designator: "U1"}}}
	if f := bapBlockPageFit([]schCluster{fitCluster("U1", boxAt(0, 0, 10, 10))}, man, nil, nil); f != nil {
		t.Fatalf("没有图框却下了判决:%+v", f)
	}
	if f := bapBlockPageFit([]schCluster{fitCluster("R9", boxAt(0, 0, 10, 10))}, man, &usable, nil); f != nil {
		t.Fatalf("一个位号都没匹配上却下了判决:%+v", f)
	}
	empty := &bapManifest{BlockID: "block.x"}
	if f := bapBlockPageFit([]schCluster{fitCluster("U1", boxAt(0, 0, 10, 10))}, empty, &usable, nil); f != nil {
		t.Fatalf("manifest 没有 placed 却下了判决:%+v", f)
	}
}

// TestSchClustersTooBig_SeparatesTheTwoDiseases:`sch clusters` 必须把「挪一挪能解」
// 与「挪不动」分成两条结论。混在一起就等于没报 —— 那正是 8 轮的生成机制。
func TestSchClustersTooBig_SeparatesTheTwoDiseases(t *testing.T) {
	usable := a4Usable()
	ko := a4Keepout()
	cs := []schCluster{
		fitCluster("U1", boxAt(60, 10, 300, 820)),   // 高 820 > 可用高 801:挪不动
		fitCluster("C1", boxAt(1000, 400, 300, 80)), // 探出右边:挪一挪能解
		fitCluster("R1", boxAt(60, 300, 100, 100)),  // 干净
	}
	got := schClustersTooBig(cs, &usable, ko)
	if len(got) != 1 || got[0].Name != "U1" {
		t.Fatalf("page-too-small 名单不对:%+v", got)
	}
	// 量不出可用区时宁可沉默 —— 猜出来的结论会掩盖真问题。
	if n := schClustersTooBig(cs, nil, nil); n != nil {
		t.Fatalf("没有图框却下了结论:%+v", n)
	}
}

// TestBapResolveOrigin_OversizeGivesTheSameSentence:估算侧与实测侧对同一件事
// 必须说同一句话。此前估算侧只说「已只按器件避让、不判图纸边界」,读起来像一句
// 内部实现说明,完全没有下一步。
func TestBapResolveOrigin_OversizeSaysWhatToDo(t *testing.T) {
	sheet := layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	// 一个宽度远超整幅的假块:两个 role 拉开 1400。
	offsets := map[string]bapRoleOffset{
		"A": {dx: 0, dy: 0},
		"B": {dx: 1400, dy: 0},
	}
	half := func(string) float64 { return 50 }
	in := bapInput{
		OriginX: 400, OriginY: 300, Sheet: &sheet,
		// 障碍铺满整个螺旋可达范围(步长 ≈ max(w,h)/2,12 圈 × 8 方向),逼螺旋与
		// 有界扫描双双落空 → 进 oversize 分支。
		Obstacles: []layoutBBox{boxAt(-100000, -100000, 200000, 200000)},
	}
	in.Block.ID = "block.legacy_wide"
	_, _, _, warns := bapResolveOrigin(in, offsets, half)
	if len(warns) == 0 {
		t.Fatal("超尺寸块没有任何警告")
	}
	joined := strings.Join(warns, "\n")
	for _, want := range []string{"估算", "再挪", "独占一页"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("估算侧的措辞少了 %q(必须与实测侧同一句话):\n%s", want, joined)
		}
	}
}
