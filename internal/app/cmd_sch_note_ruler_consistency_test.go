package app

// cmd_sch_note_ruler_consistency_test.go — 「说明能不能放进自己的区」只许有**一把尺**。
//
// 真机取证(2026-08-20,工程 ceshi,POWER 页,sy8089_buck_3v3(C1) 区,三行说明):
//
//	$ easyeda sch note --zone "sy8089_buck_3v3(C1)" --text "…三行…"
//	warning: 区 "…" 的说明带(590×55)在可扩边界内装不下这条说明(210×39)
//	note created … at (865, 441)            ← 框右边界是 848,说明被甩到框外
//	$ easyeda sch check
//	WARN note-outside-zone  说明 @(865,441) 在分区框 (258,146)..(848,496) 外
//	  修法:`sch note --zone … --x 275 --y 162.5`(说明带 … 已为它留好位置)
//
// 同一次交互、同一条说明、同一条带:一个说"装不下",另一个说"已为它留好位置"
// 还给出坐标。两把尺当面打架。根因(两半):
//
//	(1) 判定输入不同:落点求解走 scanNoteBand → noteSpotFree(判包含 **+ 占用 +
//	    图纸边距 + 图签禁区**);而 check 的处方自己按「带底 + 内缩」重算一遍坐标,
//	    判定条件**只有包含**。于是 check 能开出求解器早已拒掉的方子 —— 那个
//	    (275,162.5) 其实压在 A4 图签禁区 (468,0)..(1170,198) 上。
//	(2) 带本身是非法的:该区器件区下沿 202 已经嵌在图签安全带(顶 228)里,
//	    zone-plan 第一遍把框底"让"到器件区下沿 → 底带高度归零;
//	    reserveZoneNoteArea 却把框底一路捅到 147 **穿过图签**造出一条 55 高的带
//	    (zone-plan 自己的 titleBlockHits 当场从 0 变 1)。根因在 noteExpandFloorY:
//	    它只认"整个在框底之下"的 blocker,罩住框底的图签安全带被直接跳过。
//
// 本文件钉住修复后的配对:**求解器说能放下 ⇔ check 判定在框内、且 check 的处方
// 就是求解器的落点**。任一边改了另一边没跟上,这里当场转红。
//
// **2026-08-24 回滚说明**:67aa954 当时还加了一条「底带走不通就把说明带翻到框顶」
// 的退路,靠它让真机那条三行说明"进了框"。用户按设计正本裁定回滚 —— 正本第 2 条
// 把「区名左上、说明左下」定为版式契约(同页说明底边齐平才读得下去),第 8 条要求
// 装不下时输出 **blocked**(报出是谁、每条边各卡在哪),而不是造第四种状态偷偷挪位。
// 回滚后:根因 (2) 的修法(noteExpandFloorY 认「罩住框底」的 blocker + clamp)**独立
// 成立**,`TestRuler_NoteReservationNeverCreatesTitleBlockHit` 把它单独钉住(把判据
// 改回旧式当场转红,与顶带无关);真机那条三行说明的结局改为**如实 blocked**
// (见 TestRuler_RealMachineThreeLineNoteIsHonestlyBlocked)—— 那一区的器件区下沿
// 本来就嵌在图签安全带里,底带高度为 0、往下就是图签,它在这一页上确实没有家。

import (
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// rulerSheet 是所有用例的图纸(A4,与真机同尺寸)。
var rulerSheet = layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}

var rulerHintRe = regexp.MustCompile(`--x ([-+0-9.eE]+) --y ([-+0-9.eE]+)`)

// rulerCase 是一组「框 × 说明 × 障碍」的输入。
type rulerCase struct {
	name      string
	content   string
	fontSize  float64
	module    layoutBBox
	intruders []layoutBBox
	keepout   bool // 是否启用 A4 图签禁区
}

// rulerOutcome 是同一组输入在**两侧**分别得到的结论。
type rulerOutcome struct {
	part     partitionRect // 登记之后 planner 重算出的分区(带 + 求解器落点)
	noteW    float64
	noteH    float64
	wrapped  string
	placeX   float64
	placeY   float64
	placeOK  bool
	sheet    layoutBBox
	keepout  *layoutBBox
	allObst  []layoutBBox
	findings []checkFinding
}

// runRulerCase 跑完整条链:未登记计划 → `sch note` 侧落点求解 → 登记 → planner
// 重算 → 把说明按求解出的落点摆上去 → `sch check` 的 note-outside-zone 判定。
//
// 本地实现(不碰任何共享 helper):与 placeSchNote 的纯几何段逐行同构。
func runRulerCase(t *testing.T, c rulerCase) rulerOutcome {
	t.Helper()
	opts := defaultPartitionOpts()
	var keepout *layoutBBox
	if c.keepout {
		keepout, _ = titleBlockKeepout(&rulerSheet)
	}
	obstacles := append([]layoutBBox{c.module}, c.intruders...)

	// ① `sch note` 侧:从「本区还没登记这条说明」的计划出发。
	plan0 := planPartitionsWithNotes(rulerSheet, keepout,
		[]partitionModule{{Name: "Z", BBox: c.module, CoreBBox: c.module}}, opts, obstacles)
	if len(plan0.Partitions) != 1 {
		t.Fatalf("%s: want 1 partition, got %d", c.name, len(plan0.Partitions))
	}
	p0 := plan0.Partitions[0]
	wrapped := wrapNoteContent(c.content, c.fontSize, noteWrapWidth(p0.BBox.MaxX-p0.BBox.MinX))
	w, h := noteSizeOf(wrapped, c.fontSize)
	_, _, px, py, ok := reserveZoneNoteArea(p0.BBox, p0.notePins(), w, h, obstacles,
		rulerSheet, keepout, partitionBaseRects(plan0.Partitions, "Z"), opts.Gutter)

	// ② 登记之后 planner 重算(= `sch check` 手上的那份计划)。
	after := planPartitionsWithNotes(rulerSheet, keepout,
		[]partitionModule{{Name: "Z", BBox: c.module, CoreBBox: c.module, NoteWidth: w, NoteHeight: h}},
		opts, obstacles)
	part := after.Partitions[0]

	// ③ 说明按 ① 求出的落点落地(求解失败时用真机那种"框外兜底"坐标),
	//    再让 check 判一次归属。
	nx, ny := px, py
	if !ok {
		nx, ny = part.BBox.MaxX+noteGap, part.BBox.MaxY-noteGap-h
	}
	zones := map[string]*workflow.SchZoneClaim{"Z": {Parts: []string{"U1"}, NoteIDs: []string{"n1"}}}
	texts := []zoneMoveText{{ID: "n1", X: nx, Y: ny, Content: wrapped, FontSize: c.fontSize}}
	return rulerOutcome{part: part, noteW: w, noteH: h, wrapped: wrapped,
		placeX: px, placeY: py, placeOK: ok, sheet: rulerSheet, keepout: keepout,
		allObst:  obstacles,
		findings: noteOutsideZoneFindingsFor(after.Partitions, zones, texts)}
}

// assertOneRuler 是**配对断言**本体。
func assertOneRuler(t *testing.T, c rulerCase, o rulerOutcome) {
	t.Helper()

	// (a) 两侧对「装不装得下」的结论必须一致:`sch note` 的求解结果 ⇔ planner
	//     落进计划里的 NoteFits(check 的处方就是念它)。
	if o.placeOK != o.part.NoteFits {
		t.Fatalf("%s: 两把尺 —— sch note 侧 fit=%v,planner/check 侧 NoteFits=%v",
			c.name, o.placeOK, o.part.NoteFits)
	}

	if o.placeOK {
		// (b) 能放下 ⇒ 落点必须真的在带内、框内,而且不压任何东西 ——
		//     「判定坐标 = 落地坐标」。
		box := noteAnchorBBox(o.placeX, o.placeY, o.noteW, o.noteH)
		if !bboxContains(o.part.NoteBBox, box) {
			t.Errorf("%s: 落点 %+v 不在说明带 %+v 内", c.name, box, o.part.NoteBBox)
		}
		if !bboxContains(o.part.BBox, box) {
			t.Errorf("%s: 落点 %+v 不在分区框 %+v 内", c.name, box, o.part.BBox)
		}
		if !noteSpotFree(box, o.allObst, o.sheet, o.keepout) {
			t.Errorf("%s: 落点 %+v 压住了图元/纸边/图签禁区", c.name, box)
		}
		// (c) 能放下 ⇒ check 必须判"在框内",一条 note-outside-zone 都不许报。
		if len(o.findings) != 0 {
			t.Fatalf("%s: 求解器说放得下,check 却报 %d 条 note-outside-zone:%+v",
				c.name, len(o.findings), o.findings)
		}
		return
	}

	// (d) 放不下 ⇒ check 必须如实报,而且**绝不开一张自己都装不下的方子**。
	if len(o.findings) != 1 {
		t.Fatalf("%s: 求解器说放不下(说明落在框外),check 应恰报 1 条,got %d", c.name, len(o.findings))
	}
	msg := o.findings[0].Message
	if m := rulerHintRe.FindStringSubmatch(msg); m != nil {
		hx, _ := strconv.ParseFloat(m[1], 64)
		hy, _ := strconv.ParseFloat(m[2], 64)
		t.Fatalf("%s: 求解器拒过的位置,check 却给出处方 (%g,%g):%s", c.name, hx, hy, msg)
	}
	for _, want := range []string{"装不下", "别再原样重跑"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%s: 装不下的报文缺 %q:%s", c.name, want, msg)
		}
	}
}

// assertPrescriptionIsSolvable 是这次真机 bug 的**直接鉴别项**:check 一旦开出
// `--x/--y`,那个坐标必须是求解器自己会给的落点,并且过得了 noteSpotFree 这关
// (含图签禁区)。修复前 check 只判包含,给出的 (275,162.5) 压在图签上。
func assertPrescriptionIsSolvable(t *testing.T, c rulerCase, o rulerOutcome) {
	t.Helper()
	// 造一条**必然在框外**的说明,逼 check 出报文。
	zones := map[string]*workflow.SchZoneClaim{"Z": {Parts: []string{"U1"}, NoteIDs: []string{"n1"}}}
	far := zoneMoveText{ID: "n1", X: o.sheet.MaxX - 5, Y: o.sheet.MaxY - 5,
		Content: o.wrapped, FontSize: c.fontSize}
	fs := noteOutsideZoneFindingsFor([]partitionRect{o.part}, zones, []zoneMoveText{far})
	if len(fs) != 1 {
		t.Fatalf("%s: 框外说明应恰报 1 条,got %d", c.name, len(fs))
	}
	m := rulerHintRe.FindStringSubmatch(fs[0].Message)
	if m == nil {
		if o.part.NoteFits {
			t.Fatalf("%s: 带装得下时处方必须给出坐标:%s", c.name, fs[0].Message)
		}
		return
	}
	hx, _ := strconv.ParseFloat(m[1], 64)
	hy, _ := strconv.ParseFloat(m[2], 64)
	if hx != o.placeX || hy != o.placeY {
		t.Fatalf("%s: 处方 (%g,%g) ≠ 求解器落点 (%g,%g) —— 两把尺", c.name, hx, hy, o.placeX, o.placeY)
	}
	box := noteAnchorBBox(hx, hy, o.noteW, o.noteH)
	if !bboxContains(o.part.NoteBBox, box) || !bboxContains(o.part.BBox, box) {
		t.Fatalf("%s: 处方把说明放到了带/框外:box=%+v band=%+v frame=%+v",
			c.name, box, o.part.NoteBBox, o.part.BBox)
	}
	if !noteSpotFree(box, o.allObst, o.sheet, o.keepout) {
		t.Fatalf("%s: 处方坐标 (%g,%g) 压住图元/图签禁区 —— check 只判了包含,没判占用", c.name, hx, hy)
	}
}

// ── 随机配对:框尺寸 × 说明行数/字号 × 障碍 × 有无图签 ────────────────────────
func TestRuler_NoteFitAndCheckAgreeRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(20260820)) // 固定种子:失败可复现
	fits, misses := 0, 0
	for i := 0; i < 400; i++ {
		x0 := 60 + rng.Float64()*400
		y0 := 40 + rng.Float64()*500
		w := 60 + rng.Float64()*450
		h := 60 + rng.Float64()*260
		mod := layoutBBox{MinX: x0, MinY: y0, MaxX: x0 + w, MaxY: y0 + h}
		lines := 1 + rng.Intn(6)
		font := []float64{9, 10, 11}[rng.Intn(3)]
		var intruders []layoutBBox
		if rng.Intn(2) == 0 { // 一半用例带一条伸进带里的邻区桩线
			ix := mod.MinX + rng.Float64()*math.Max(1, w-40)
			iy := mod.MinY - 20 - rng.Float64()*90
			intruders = append(intruders, layoutBBox{ix, iy, ix + 40 + rng.Float64()*200, iy + 50})
		}
		c := rulerCase{
			name:     "rand#" + strconv.Itoa(i),
			content:  noteContentOf(lines),
			fontSize: font,
			module:   mod, intruders: intruders,
			keepout: rng.Intn(2) == 0,
		}
		o := runRulerCase(t, c)
		assertOneRuler(t, c, o)
		assertPrescriptionIsSolvable(t, c, o)
		if o.placeOK {
			fits++
		} else {
			misses++
		}
		if t.Failed() {
			t.Fatalf("%s 失败,输入:module=%+v lines=%d font=%.0f keepout=%v intruders=%+v",
				c.name, mod, lines, font, c.keepout, intruders)
		}
	}
	// 鉴别力自证:两种结论都得出现过,否则这批随机用例只覆盖了一条分支。
	if fits == 0 || misses == 0 {
		t.Fatalf("随机用例失去鉴别力:fits=%d misses=%d(两侧都该出现)", fits, misses)
	}
	t.Logf("配对通过:%d 组能放下 / %d 组放不下", fits, misses)
}

// ── 真机那条三行说明:如实 blocked,而不是"大概摆了一下" ──────────────────────
//
// 复刻真机几何:A4 图签禁区 (468,0)..(1170,198)、安全带顶 228,器件区下沿 202
// **已经嵌在安全带里**,zone-plan 第一遍于是把框底"让"到器件区下沿(内容一寸
// 不让)—— 底带高度当场归零,而框底再往下一寸就是图签。
//
// 这一区在底带方向上**结构上没有地方**。设计正本第 2 条把「说明左下」定为版式
// 契约(同页所有说明底边齐平),第 8 条要求这种情形输出 **blocked**:报出是谁、
// 每条边各卡在哪、出路是什么,交人做区内收敛或拆页 —— 而不是造第四种状态把说明
// 挪到框顶(67aa954 那条上翻退路,2026-08-24 已回滚)。
//
// 所以本用例钉的是**三件事**:
//   - 结论如实:两侧都说放不下,check 恰报 1 条且**不开 --x/--y 的方子**;
//   - 报文可执行:点名区、给出纵向差、给出两条出路;
//   - 框不撒谎:框底一寸都没探进图签 keep-out(根因 B 的直接鉴别项)。
func TestRuler_RealMachineThreeLineNoteIsHonestlyBlocked(t *testing.T) {
	c := rulerCase{
		name:     "sy8089_buck_3v3(C1)",
		module:   layoutBBox{282, 202, 824, 442},
		content:  "SY8089 同步降压 +5V→+3V3 / 2A\nFB 分压 R1 10k / R2 453k;EN 接 VIN 常开\n输出 L1 22µH + C3 22µF, C1 100nF 旁路",
		fontSize: 10,
		keepout:  true,
	}
	o := runRulerCase(t, c)
	t.Logf("三行说明 %.0f×%.0f:fits=%v,带 %+v,框 %+v",
		o.noteW, o.noteH, o.placeOK, o.part.NoteBBox, o.part.BBox)

	if o.placeOK {
		t.Fatalf("这一区的底带高度为 0、往下就是图签,不该报「放得下」;落点 (%g,%g) 带 %+v",
			o.placeX, o.placeY, o.part.NoteBBox)
	}
	// 两侧结论一致 + check 如实报 1 条、绝不开求解器拒过的方子。
	assertOneRuler(t, c, o)
	assertPrescriptionIsSolvable(t, c, o)

	// 报文必须回答正本第 8 条的三问:谁 / 卡在哪条边 / 出路。
	msg := o.findings[0].Message
	// 报出**是谁**:runRulerCase 把这一区登记成 "Z"(真机是 sy8089_buck_3v3(C1))。
	for _, want := range []string{`区 "Z"`, "纵向差", "区内收敛", "拆页", "zone-arrange", "page-new"} {
		if !strings.Contains(msg, want) {
			t.Errorf("blocked 报文缺 %q:%s", want, msg)
		}
	}
	// 根因 B 的直接鉴别项:框底一寸都不许探进图签 keep-out / 安全带。
	if o.keepout == nil {
		t.Fatal("fixture 失效:这条用例必须有图签禁区")
	}
	if f := o.part.BBox; f.MinY < o.keepout.MaxY && f.MaxX > o.keepout.MinX && f.MinX < o.keepout.MaxX {
		t.Fatalf("为说明预留把框底捅进了图签:frame %+v vs keepout %+v", f, *o.keepout)
	}
	if safe := inflatedTitleKeepout(o.keepout); safe != nil && boxesOverlap(o.part.BBox, *safe) &&
		o.part.BBox.MinY < c.module.MinY {
		t.Fatalf("框底探进了图签安全带 %+v(器件区下沿 %.0f):frame %+v", *safe, c.module.MinY, o.part.BBox)
	}
}

// ── 负对照 1:真的装不下(10 行说明 + 四面堵死)仍要如实报 ────────────────────
func TestRuler_TrulyUnfittableStillReported(t *testing.T) {
	c := rulerCase{
		name:     "boxed-in",
		module:   layoutBBox{200, 300, 500, 460},
		content:  noteContentOf(10),
		fontSize: 11,
		intruders: []layoutBBox{
			{100, 0, 700, 292},   // 下方封死
			{100, 468, 700, 813}, // 上方封死
		},
	}
	o := runRulerCase(t, c)
	if o.placeOK {
		t.Fatalf("四面堵死时必须如实失败,却给出落点 (%g,%g);band=%+v", o.placeX, o.placeY, o.part.NoteBBox)
	}
	assertOneRuler(t, c, o) // 含:check 必须报 1 条,且不许给 --x/--y
	assertPrescriptionIsSolvable(t, c, o)
}

// ── 负对照 2:为说明预留绝不许自己撑出 titleBlockHits ─────────────────────────
//
// 修复前 noteExpandFloorY 会跳过"罩住框底"的图签安全带,把框底从 202 捅到 147,
// zone-plan 的 titleBlockHits 当场 0 → 1(自己造出来的违规,zone-draw 会拒画)。
func TestRuler_NoteReservationNeverCreatesTitleBlockHit(t *testing.T) {
	opts := defaultPartitionOpts()
	keepout, _ := titleBlockKeepout(&rulerSheet)
	mod := layoutBBox{282, 202, 824, 442}
	content := noteContentOf(3)
	w, h := noteSizeOf(content, 10)

	before := planPartitionsWithNotes(rulerSheet, keepout,
		[]partitionModule{{Name: "Z", BBox: mod, CoreBBox: mod}}, opts, []layoutBBox{mod})
	after := planPartitionsWithNotes(rulerSheet, keepout,
		[]partitionModule{{Name: "Z", BBox: mod, CoreBBox: mod, NoteWidth: w, NoteHeight: h}},
		opts, []layoutBBox{mod})

	if after.Validation.TitleBlockHits > before.Validation.TitleBlockHits {
		t.Fatalf("为说明预留自己撑出了 titleBlockHits:%d → %d(框 %+v → %+v)",
			before.Validation.TitleBlockHits, after.Validation.TitleBlockHits,
			before.Partitions[0].BBox, after.Partitions[0].BBox)
	}
	// 框底一寸都不许再往图签里探。
	if raw := after.Partitions[0].BBox; keepout != nil && raw.MinY < keepout.MaxY &&
		raw.MaxX > keepout.MinX && raw.MinX < keepout.MaxX {
		t.Fatalf("框底探进了图签:frame %+v vs keepout %+v", raw, *keepout)
	}
}

// ── 幂等:说明登记前后,两侧算出的框/带/落点必须逐字段相同 ────────────────────
//
// 两档都要覆盖(否则这条测试只证明了一种结论会收敛):
//   - 装得下:落点必须逐字段相同(底带贴底 / 窄框横向扩边两种形态);
//   - blocked:两侧必须**同样**判 blocked,且 planner 不许悄悄落一个锚点进计划
//     —— 计划里躺着坐标而 NoteFits=false,就是下一轮「check 开出求解器拒过的
//     方子」的种子。原「顶带」那一档随上翻退路一并回滚(2026-08-24)。
func TestRuler_PlaceThenPlanConverges(t *testing.T) {
	for _, c := range []rulerCase{
		{name: "底带贴底", module: layoutBBox{260, 452, 647, 700}, content: noteContentOf(3), fontSize: 10},
		// 窄框:框宽 68 装不下任何可读说明 → 求解器横向扩边,两侧必须扩得一样。
		{name: "窄框横向扩边", module: layoutBBox{420, 452, 488, 620}, content: noteContentOf(2), fontSize: 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := runRulerCase(t, c)
			if !o.placeOK {
				t.Fatalf("fixture 失效:这条说明本该放得下;band=%+v", o.part.NoteBBox)
			}
			if o.part.NoteAnchor[0] != o.placeX || o.part.NoteAnchor[1] != o.placeY {
				t.Fatalf("planner 重算的落点 %v ≠ sch note 侧 (%g,%g)",
					o.part.NoteAnchor, o.placeX, o.placeY)
			}
		})
	}
	t.Run("blocked 也要收敛", func(t *testing.T) {
		c := rulerCase{name: "器件嵌在图签安全带里", module: layoutBBox{282, 202, 824, 442},
			content: noteContentOf(3), fontSize: 10, keepout: true}
		o := runRulerCase(t, c)
		if o.placeOK || o.part.NoteFits {
			t.Fatalf("底带归零、下方是图签:两侧都该判 blocked;fit=%v NoteFits=%v", o.placeOK, o.part.NoteFits)
		}
		if o.part.NoteAnchor != [2]float64{} {
			t.Fatalf("blocked 的分区不许在计划里留落点坐标:%v", o.part.NoteAnchor)
		}
	})
}
