package app

// cmd_sch_zone_tighten_test.go — 「分区框收紧」的正/负/红线对照。
//
// ── 缺陷(2026-08-24 真机 esp32Mini E2E,连跑两轮完全一致)────────────────────
//
//	$ easyeda sch zone-draw --project ceshi --doc MCU_IO --mode partition
//	partition plan has violations {SheetOverflow:0 PartitionOverlap:3 TitleBlockHits:1 …}
//	  — refusing to draw overlapping/out-of-sheet annotations
//	  ⚠ 3 对分区框重叠:两个区的成员体积本身就交叠 —— `sch zone-arrange --apply` 重排…
//
// 后果链:拒画 → 页面没框 → `sch check` 报 missing-partition → `sch gate --strict`
// FAIL → 交付被卡。而唯一出路是整页重排(重、有风险,USB_DEBUG 那页还直接 blocked)。
//
// **拒画本身是对的**(正本第 6/8 条:判定 = 生成同一把尺,过不了就 blocked,绝不
// 出半成品),真正的缺陷有两处:
//
//	① 报文里那句「两个区的成员体积本身就交叠」**经常不是真的**。框 = 内容 + 边距
//	   24 + 区名带 30 + 说明带 42;两个内容明明分得开的模块,只要间距小于这些
//	   **预留**之和,框就撞上。规划器对图签、对纸边一直执行「预留可让、内容一寸
//	   不让」,**唯独对邻区不让** —— 同一条规矩少执行了一处,于是把「余量对撞」
//	   判成了「布局有病」。
//	② 真的没地方时,报文给不出**挪多少**,只能让人跑整页重排。
//
// 修法对应两条:tightenPartitionFrames(邻框也吃可让的余量,先各拿下界、余量对半分)
// + 逐对可执行明细(是谁、重叠多少、内容真交叠还是预留顶住、一条能抄去跑的命令)。
//
// ── 红线 ──────────────────────────────────────────────────────────────────
//
// **收紧一寸都不许放宽「不画重叠框」这条判据。** 由三层钉住:
//
//	· TestTighten_ContentOverlapStillRefused         内容真交叠 → 照旧拒画
//	· TestTighten_GatePassImpliesZeroDrawnOverlap    随机对照:gate 一旦放行,
//	                                                 **实际发给 SDK 的那些矩形**逐对零重叠、零压图签
//	· TestTighten_NeverIntroducesAViolation          随机对照:收紧后六项计数逐项 ≤ 收紧前

import (
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ── 工具 ─────────────────────────────────────────────────────────────────────

// ztUntightened 复刻**收紧之前**的几何:逐个模块单独规划(单模块页没有邻居,
// 收紧天然是 no-op),再合起来验证。变异对照据此证明 fixture 真的复现了缺陷。
func ztUntightened(sheet layoutBBox, keepout *layoutBBox, mods []partitionModule, opts partitionOpts) partitionPlan {
	plan := partitionPlan{Sheet: sheet, Keepout: keepout}
	for _, m := range mods {
		one := planPartitions(sheet, keepout, []partitionModule{m}, opts)
		if len(one.Partitions) != 1 {
			panic("ztUntightened: 单模块该出一个分区")
		}
		plan.Partitions = append(plan.Partitions, one.Partitions[0])
	}
	plan.Validation = validatePartitions(plan, mods, keepout)
	return plan
}

func ztPartitionByName(plan partitionPlan, name string) (partitionRect, bool) {
	for _, p := range plan.Partitions {
		if strInSlice(p.Modules, name) {
			return p, true
		}
	}
	return partitionRect{}, false
}

// ztOverlapPairs 逐对列出真正相交的框(判定用的就是生产的 boxesOverlap)。
func ztOverlapPairs(ps []partitionRect) []string {
	var out []string
	for i := 0; i < len(ps); i++ {
		for j := i + 1; j < len(ps); j++ {
			if boxesOverlap(ps[i].BBox, ps[j].BBox) {
				out = append(out, fmt.Sprintf("%s↔%s", partitionZoneName(ps[i]), partitionZoneName(ps[j])))
			}
		}
	}
	return out
}

// ── 正对照 ①:横向相邻 —— 顶住的只是边距,收紧后照画 ──────────────────────────

func TestTighten_SideBySideReservationOverlapNowDraws(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	// 两块内容左右相邻,中间只有 20 单位空地:各自 24 的边距一对撞就重叠 28。
	// 内容本身分得开 —— 这是「余量对撞」,不是布局有病。
	mods := []partitionModule{
		{Name: "left", BBox: layoutBBox{200, 400, 400, 600}, CoreBBox: layoutBBox{210, 410, 390, 590}},
		{Name: "right", BBox: layoutBBox{420, 400, 620, 600}, CoreBBox: layoutBBox{430, 410, 610, 590}},
	}

	// 变异对照:收紧之前必须复现重叠,否则这份 fixture 证明不了任何事。
	before := ztUntightened(sheet, nil, mods, opts)
	if before.Validation.PartitionOverlap == 0 {
		t.Fatalf("变异对照失效:未收紧的框居然不重叠(%v)—— fixture 不再复现缺陷", ztOverlapPairs(before.Partitions))
	}

	plan := planPartitions(sheet, nil, mods, opts)
	if got := ztOverlapPairs(plan.Partitions); len(got) > 0 {
		t.Fatalf("收紧后仍重叠 %v:%s", got, plan.Validation.counters())
	}
	if err := partitionDrawGate(plan); err != nil {
		t.Fatalf("内容分得开的页必须画得出框,却被拒:%v", err)
	}
	// 让掉的必须是**边距**,内容一寸不让:框恒包住自己的内容。
	for _, p := range plan.Partitions {
		if !bboxContains(p.BBox, p.ContentBBox) {
			t.Errorf("%s:收紧把内容切出去了 %s ⊅ %s", partitionZoneName(p), bboxText(p.BBox), bboxText(p.ContentBBox))
		}
		if !p.Tightened {
			t.Errorf("%s:让过步却没标 Tightened —— 让步必须可见", partitionZoneName(p))
		}
	}
	// 两条版式带一寸不减(正本第 2 条的读图契约)。
	for _, p := range plan.Partitions {
		if band := p.TitleBBox.MaxY - p.TitleBBox.MinY; band < opts.TitleBand-1e-9 {
			t.Errorf("%s:区名带被收薄成 %.0f(标称 %.0f)", partitionZoneName(p), band, opts.TitleBand)
		}
		if band := p.NoteBBox.MaxY - p.NoteBBox.MinY; band < opts.NoteBand-1e-9 {
			t.Errorf("%s:说明带被收薄成 %.0f(标称 %.0f)", partitionZoneName(p), band, opts.NoteBand)
		}
	}
	// 真的画得出来:发给 SDK 的矩形逐对不相交。
	rendered := renderedZoneRectangleBBoxes(t, buildPartitionDrawJS(plan, defaultPartitionZoneFontSize, "#AA00AA"))
	if len(rendered) != 2 {
		t.Fatalf("draw 发出 %d 个矩形,want 2", len(rendered))
	}
	if boxesOverlap(rendered[0], rendered[1]) {
		t.Fatalf("红线破了:画出去的两个矩形相交 %s / %s", bboxText(rendered[0]), bboxText(rendered[1]))
	}
	t.Logf("收紧前 %v → 收紧后零重叠;框:%s / %s", ztOverlapPairs(before.Partitions),
		bboxText(plan.Partitions[0].BBox), bboxText(plan.Partitions[1].BBox))
}

// ── 正对照 ②:上下相叠 —— 空地够放两条带,「先拿下界再对半分」必须找得出切点 ──
//
// 这一条专防「在空地中点一刀切」那种写法:竖着叠的两区,下界之间还剩 14 的余量
// (**存在**合法切点),中点却落在上区够不着的地方,于是残留重叠、整页照样拒画 ——
// 有解却报无解。
func TestTighten_StackedWithRoomForBothBandsResolves(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	mods := []partitionModule{
		{Name: "upper", BBox: layoutBBox{200, 580, 500, 730}, CoreBBox: layoutBBox{210, 590, 490, 720}},
		{Name: "lower", BBox: layoutBBox{200, 300, 500, 470}, CoreBBox: layoutBBox{210, 310, 490, 460}},
	}
	// 竖叠要 96 才收得开(边距 24 + 说明带 + 区名带,见 partitionFrameFloor);
	// 这份 fixture 给 110 —— 够,但第一遍的框仍然是重叠的。
	if gap := 580.0 - 470.0; gap < partitionContentPad+opts.NoteBand+opts.TitleBand {
		t.Fatalf("fixture 前提错了:空地 %.0f 收不开(需要 %.0f)", gap,
			partitionContentPad+opts.NoteBand+opts.TitleBand)
	}
	before := ztUntightened(sheet, nil, mods, opts)
	if before.Validation.PartitionOverlap == 0 {
		t.Fatalf("变异对照失效:未收紧居然不重叠 —— fixture 不复现缺陷")
	}
	plan := planPartitions(sheet, nil, mods, opts)
	if got := ztOverlapPairs(plan.Partitions); len(got) > 0 {
		t.Fatalf("空地够放两条带却没切开(说明退回了「中点一刀切」):%v\n上 %s\n下 %s",
			got, bboxText(plan.Partitions[0].BBox), bboxText(plan.Partitions[1].BBox))
	}
	if err := partitionDrawGate(plan); err != nil {
		t.Fatalf("该画得出来,却被拒:%v", err)
	}
	// 两条带都必须完整保留(切点是「各拿下界 + 余量对半」,不是把带削薄换来的)。
	up, _ := ztPartitionByName(plan, "upper")
	lo, _ := ztPartitionByName(plan, "lower")
	if h := up.NoteBBox.MaxY - up.NoteBBox.MinY; h < opts.NoteBand-1e-9 {
		t.Errorf("上区说明带被削薄成 %.0f", h)
	}
	if h := lo.TitleBBox.MaxY - lo.TitleBBox.MinY; h < opts.TitleBand-1e-9 {
		t.Errorf("下区区名带被削薄成 %.0f", h)
	}
}

// ── 负对照(红线):内容真交叠 —— 照旧拒画,且要说清是哪一种病 ─────────────────

func TestTighten_ContentOverlapStillRefused(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	mods := []partitionModule{
		{Name: "A", BBox: layoutBBox{300, 300, 620, 560}, CoreBBox: layoutBBox{320, 320, 600, 540}},
		{Name: "B", BBox: layoutBBox{500, 380, 760, 620}, CoreBBox: layoutBBox{520, 400, 740, 600}},
	}
	plan := planPartitions(sheet, nil, mods, opts)
	if plan.Validation.PartitionOverlap == 0 {
		t.Fatalf("红线破了:体积真的互相压,收紧却把重叠「收没了」——收紧只许让边距,不许动内容")
	}
	err := partitionDrawGate(plan)
	if err == nil {
		t.Fatal("红线破了:重叠的计划被放行画框")
	}
	msg := err.Error()
	// 顺序由绘制序决定(视觉上方优先),这里只要求两个区都被点名。
	for _, want := range []string{"PartitionOverlap", "分区框重叠", `区 "A"`, `区 "B"`, "↔", "成员体积本身就交叠"} {
		if !strings.Contains(msg, want) {
			t.Errorf("拒绝理由缺 %q(blocked 必须报出是谁、卡在哪):\n%s", want, msg)
		}
	}
	// 内容真交叠时不许开「挪一挪就好」以外的假方子,但仍要给出最小挪动。
	if !strings.Contains(msg, "sch zone move --zone") {
		t.Errorf("blocked 没给可执行的下一步:\n%s", msg)
	}
	t.Logf("负对照如期拒绝:\n%v", err)
}

// ── 可执行性验收:blocked 时报出的「最小挪动」照着挪,必须真的解得开 ─────────────
//
// 判据的价值不在报数,在给出能执行的下一步。这一条把报文当**接口**来测:
// 从明细里把命令解析出来 → 按它挪模块 → 重新规划 → 那一对必须不再重叠。

var ztMoveRE = regexp.MustCompile(`sch zone move --zone ([^\s]+) --(dx|dy) ([-+0-9.]+)`)

func TestTighten_BlockedAdviceIsExecutable(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	// 上下相叠,空地只有 50 —— 两条带要 72,**真的**装不下:如实 blocked。
	// 但「还差多少」是算得出来的,报出的挪动量必须足够。
	mods := []partitionModule{
		{Name: "upper", BBox: layoutBBox{200, 520, 500, 700}, CoreBBox: layoutBBox{210, 530, 490, 690}},
		{Name: "lower", BBox: layoutBBox{200, 300, 500, 470}, CoreBBox: layoutBBox{210, 310, 490, 460}},
	}
	plan := planPartitions(sheet, nil, mods, opts)
	if plan.Validation.PartitionOverlap != 1 {
		t.Fatalf("前提没成立:该剩 1 对重叠,得到 %d(%s)", plan.Validation.PartitionOverlap, plan.Validation.counters())
	}
	if len(plan.Validation.PartitionOverlapDetail) != 1 {
		t.Fatalf("每一对都要单独成条:%v", plan.Validation.PartitionOverlapDetail)
	}
	detail := plan.Validation.PartitionOverlapDetail[0]
	if !strings.Contains(detail, "并不交叠") {
		t.Errorf("这一对的内容并不交叠,报文却说成体积互压(那会把人指向错误的修法):%s", detail)
	}
	m := ztMoveRE.FindStringSubmatch(detail)
	if m == nil {
		t.Fatalf("报文里没有可执行的挪动命令:%s", detail)
	}
	zone, axis := m[1], m[2]
	d, err := strconv.ParseFloat(m[3], 64)
	if err != nil {
		t.Fatalf("挪动量解析失败 %q:%v", m[3], err)
	}
	if d == 0 {
		t.Fatalf("报文给了 0 位移(等于没给):%s", detail)
	}
	moved := make([]partitionModule, len(mods))
	copy(moved, mods)
	found := false
	for i := range moved {
		if moved[i].Name != zone {
			continue
		}
		found = true
		dx, dy := 0.0, d
		if axis == "dx" {
			dx, dy = d, 0
		}
		moved[i].BBox = shiftBBox(moved[i].BBox, dx, dy)
		moved[i].CoreBBox = shiftBBox(moved[i].CoreBBox, dx, dy)
	}
	if !found {
		t.Fatalf("报文点名的区 %q 不在本页(抄下来就跑不通):%s", zone, detail)
	}
	after := planPartitions(sheet, nil, moved, opts)
	if got := ztOverlapPairs(after.Partitions); len(got) > 0 {
		t.Fatalf("照着报文挪完仍然重叠 %v —— 报出来的最小挪动是个跑不通的方子\n明细:%s", got, detail)
	}
	if err := partitionDrawGate(after); err != nil {
		t.Fatalf("照着报文挪完仍画不出框:%v\n明细:%s", err, detail)
	}
	t.Logf("blocked → 抄一条 `sch zone move --zone %s --%s %.0f` → 画得出来。明细:\n%s", zone, axis, d, detail)
}

// ── 干净页:收紧必须是**零改动** ───────────────────────────────────────────────
//
// zone-arrange 落地后的页(各区按 gutter 隔开)本来就不重叠,收紧一个字都不许改 ——
// 否则 zone-plan 的框就不再逐字段等于断言③ 量出来的实测框(那是既有的配对契约)。
func TestTighten_NoOpOnAlreadyCleanPage(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	keepout, _ := titleBlockKeepout(&sheet)
	_, _, mods := zpLandedMCUIO(t, opts)

	plan := planPartitions(sheet, keepout, mods, opts)
	for _, p := range plan.Partitions {
		if p.Tightened {
			t.Errorf("%s:干净页居然被收紧了 —— 不重叠就一个字不许改", partitionZoneName(p))
		}
	}
	before := ztUntightened(sheet, keepout, mods, opts)
	for _, p := range plan.Partitions {
		q, ok := ztPartitionByName(before, p.Modules[0])
		if !ok {
			t.Fatalf("%s 不在对照计划里", p.Modules[0])
		}
		if p.BBox != q.BBox {
			t.Errorf("%s:收紧改动了干净页的框 %s → %s", p.Modules[0], bboxText(q.BBox), bboxText(p.BBox))
		}
	}
}

// ── 随机对照 ①:收紧不许**新增**任何一项违规 ──────────────────────────────────

func ztRandomModules(rnd *rand.Rand, n int) []partitionModule {
	out := make([]partitionModule, 0, n)
	for i := 0; i < n; i++ {
		w := 60 + rnd.Float64()*260
		h := 60 + rnd.Float64()*260
		x := 40 + rnd.Float64()*(1100-w)
		y := 40 + rnd.Float64()*(760-h)
		b := layoutBBox{MinX: x, MinY: y, MaxX: x + w, MaxY: y + h}
		core := layoutBBox{MinX: b.MinX + 5, MinY: b.MinY + 5, MaxX: b.MaxX - 5, MaxY: b.MaxY - 5}
		out = append(out, partitionModule{Name: fmt.Sprintf("z%d", i), BBox: b, CoreBBox: core})
	}
	return out
}

func TestTighten_NeverIntroducesAViolation(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	keepout, _ := titleBlockKeepout(&sheet)
	rnd := rand.New(rand.NewSource(20260824))
	for it := 0; it < 400; it++ {
		mods := ztRandomModules(rnd, 2+rnd.Intn(5))
		before := ztUntightened(sheet, keepout, mods, opts).Validation
		after := planPartitions(sheet, keepout, mods, opts)
		got := after.Validation
		type pair struct {
			name string
			b, a int
		}
		for _, p := range []pair{
			{"sheetOverflow", before.SheetOverflow, got.SheetOverflow},
			{"partitionOverlap", before.PartitionOverlap, got.PartitionOverlap},
			{"titleBlockHits", before.TitleBlockHits, got.TitleBlockHits},
			{"moduleOutsideZone", before.ModuleOutsideZone, got.ModuleOutsideZone},
			{"labelCollisions", before.LabelCollisions, got.LabelCollisions},
			{"sheetMarginHits", before.SheetMarginHits, got.SheetMarginHits},
		} {
			if p.a > p.b {
				t.Fatalf("iter %d:收紧把 %s 从 %d 变成了 %d —— 收紧只许减违规,不许造违规\nmods=%+v",
					it, p.name, p.b, p.a, mods)
			}
		}
		// 框恒包住自己的内容;**框底一寸不让**(说明带是版式契约,见 partitionFrameFloor),
		// 所以收紧前后的框底与说明带必须逐字段相同。
		beforePlan := ztUntightened(sheet, keepout, mods, opts)
		for _, p := range after.Partitions {
			if !bboxContains(p.BBox, p.ContentBBox) {
				t.Fatalf("iter %d:%s 的框 %s 没罩住内容 %s", it, partitionZoneName(p), bboxText(p.BBox), bboxText(p.ContentBBox))
			}
			q, ok := ztPartitionByName(beforePlan, p.Modules[0])
			if !ok {
				t.Fatalf("iter %d:%s 不在对照计划里", it, p.Modules[0])
			}
			if p.BBox.MinY != q.BBox.MinY {
				t.Fatalf("iter %d:%s 的框底被收紧动了 %.1f → %.1f —— 说明带不许被削",
					it, p.Modules[0], q.BBox.MinY, p.BBox.MinY)
			}
			// 带**高**不许减(带宽随框宽走,窄了由 `sch note` 的横向扩边/如实
			// blocked 那条路管,那是另一件事)。
			gotH := p.NoteBBox.MaxY - p.NoteBBox.MinY
			wantH := q.NoteBBox.MaxY - q.NoteBBox.MinY
			if gotH < wantH-1e-9 {
				t.Fatalf("iter %d:%s 的说明带被收薄 %.1f → %.1f", it, p.Modules[0], wantH, gotH)
			}
		}
	}
}

// ── 随机对照 ②(红线):gate 一旦放行,画出去的矩形逐对零重叠、零压图签 ──────────
//
// 判定用的是 plan,画的是 buildPartitionDrawJS 发给 SDK 的坐标 —— 这里量的是**后者**,
// 于是「判定与落地同一把尺」这条不靠注释靠测试。
func TestTighten_GatePassImpliesZeroDrawnOverlap(t *testing.T) {
	opts := defaultPartitionOpts()
	sheet := zpSheet()
	keepout, _ := titleBlockKeepout(&sheet)
	safe := inflatedTitleKeepout(keepout)
	rnd := rand.New(rand.NewSource(20260825))
	passed, blocked := 0, 0
	for it := 0; it < 400; it++ {
		mods := ztRandomModules(rnd, 2+rnd.Intn(5))
		plan := planPartitions(sheet, keepout, mods, opts)
		if err := partitionDrawGate(plan); err != nil {
			blocked++
			continue
		}
		passed++
		rendered := renderedZoneRectangleBBoxes(t, buildPartitionDrawJS(plan, defaultPartitionZoneFontSize, "#AA00AA"))
		if len(rendered) != len(plan.Partitions) {
			t.Fatalf("iter %d:发出 %d 个矩形,计划 %d 个", it, len(rendered), len(plan.Partitions))
		}
		for i := 0; i < len(rendered); i++ {
			if !bboxContains(sheet, rendered[i]) {
				t.Fatalf("iter %d:画出去的框跑出纸面 %s", it, bboxText(rendered[i]))
			}
			if safe != nil && boxesOverlap(rendered[i], *keepout) {
				t.Fatalf("iter %d:画出去的框压在图签上 %s", it, bboxText(rendered[i]))
			}
			for j := i + 1; j < len(rendered); j++ {
				if boxesOverlap(rendered[i], rendered[j]) {
					t.Fatalf("iter %d:红线破了 —— gate 放行却画出相交的两个框 %s / %s",
						it, bboxText(rendered[i]), bboxText(rendered[j]))
				}
			}
		}
	}
	if passed == 0 || blocked == 0 {
		t.Fatalf("对照没意义:pass=%d blocked=%d(两边都要有样本)", passed, blocked)
	}
	t.Logf("随机 400 页:%d 页放行(逐对零重叠)、%d 页 blocked", passed, blocked)
}

// ── check 侧的出路也必须是轻的那一条 ─────────────────────────────────────────
//
// 死锁的另一半在 `sch check`:画不出框 → missing-partition → `sch gate --strict` FAIL。
// 判据本身一个字不放宽(有组不等于画了框),但**它开的方子**不许只有整页重排 ——
// 那正是真机上把交付卡死的那条路。
func TestPartitionFinding_PointsAtTheLightWayOutFirst(t *testing.T) {
	got := partitionFindingForZones(12, 0, 0, 0, 4)
	var msg string
	for _, f := range got {
		if f.Type == "missing-partition" {
			msg = f.Message
		}
	}
	if msg == "" {
		t.Fatalf("已归组但没画框的页必须照报 missing-partition(判据一个字不放宽):%+v", got)
	}
	for _, want := range []string{"zone-plan", "sch zone move --zone", "最后一档"} {
		if !strings.Contains(msg, want) {
			t.Errorf("处方缺 %q —— 出路要先给轻的那条:\n%s", want, msg)
		}
	}
	// 画过框的页照旧闭嘴(不许因为改了报文顺手把判据放宽)。
	for _, f := range partitionFindingForZones(12, 4, 4, 8, 4) {
		if f.Type == "missing-partition" {
			t.Errorf("画过框的页不该报 missing-partition:%s", f.Message)
		}
	}
}

// ── 收紧的算术本体:先拿下界,余量对半分 ───────────────────────────────────────

func TestPartitionGapSplit_FloorsFirstThenHalveTheSlack(t *testing.T) {
	// 内容 [0,100] 与 [180,280](空地 80);下界各自向空地里伸 20 / 30(合计 50),
	// 余量 30 对半 → 切点 = 120+15 = 135 = 150-15。
	ca := layoutBBox{MinX: 0, MinY: 0, MaxX: 100, MaxY: 100}
	cb := layoutBBox{MinX: 180, MinY: 0, MaxX: 280, MaxY: 100}
	fa := layoutBBox{MinX: -20, MinY: 0, MaxX: 120, MaxY: 100}
	fb := layoutBBox{MinX: 150, MinY: 0, MaxX: 300, MaxY: 100}
	axis, aFirst, lo, hi := partitionGapSplit(ca, cb, fa, fb)
	if axis != "x" || !aFirst || lo != 135 || hi != 135 {
		t.Fatalf("axis=%q aFirst=%v lo=%v hi=%v,want x/true/135/135", axis, aFirst, lo, hi)
	}
	// 顺序无关:交换只翻 aFirst。
	axis2, aFirst2, lo2, hi2 := partitionGapSplit(cb, ca, fb, fa)
	if axis2 != axis || aFirst2 == aFirst || lo2 != lo || hi2 != hi {
		t.Fatalf("交换 a/b 换了答案:%q/%v/%v/%v vs %q/%v/%v/%v", axis2, aFirst2, lo2, hi2, axis, aFirst, lo, hi)
	}
	// 下界本身就装不下(合计 > 空地):各停在自己的下界,残留量 = 真实差额。
	fa2 := layoutBBox{MinX: -20, MinY: 0, MaxX: 160, MaxY: 100}
	_, _, lo3, hi3 := partitionGapSplit(ca, cb, fa2, fb)
	if lo3 != 160 || hi3 != 150 {
		t.Fatalf("装不下时该各停在下界(160/150),得到 %v/%v", lo3, hi3)
	}
	if math.Abs((lo3-hi3)-10) > 1e-9 {
		t.Fatalf("残留量该等于差额 10,得到 %v", lo3-hi3)
	}
	// 内容两轴都交叠:没有空地可分。
	if axis, _, _, _ := partitionGapSplit(ca, layoutBBox{50, 50, 150, 150}, fa, fb); axis != "" {
		t.Fatalf("内容交叠时不该给出可让的轴,得到 %q", axis)
	}
}
