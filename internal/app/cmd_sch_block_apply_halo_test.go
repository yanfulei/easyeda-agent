package app

import (
	"fmt"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/blocks"
)

// ── LED 块必压 WROOM 模组(确定性复现)────────────────────────────────────────
//
// 2026-08-20 esp32Mini 端到端**两轮逐字一致**的缺陷:MCU_IO 页先落
// block.esp32s3_wroom1_module,再落 block.led_indicator_gpio,`sch clusters` 必报
//
//	clusters ✗ 虚拟组 LED1 ↔ U2 的图元重叠 6×2 —— 两组各自的 marker/桩线压在一起了
//
// 根因是**两把尺**:落点求解只量器件本体(障碍表只喂 part 的 bbox、块自己也只按
// 本体估算),而组重叠判据量的是 L1 全图元(器件 ∪ 它自己的 marker/桩线)。于是
// 螺旋"合法"地把 LED 块放进 U2 本体与 U2 marker 之间的那条缝里。
//
// 下面的坐标全部来自真机 components.list 快照(audit 2026-08-24T03:27:51,
// includeBBox+includePins),不是造的:U2 本体 x=[684.5,755.5],而它的 MCU_RX
// netport 本体 x=[534.5,565.5] —— 加上名字带(acPortTotalLen)一路探到 x≈490。
func wroomPageComps() []layoutComp {
	f := func(t, net string, x, y, minX, minY, maxX, maxY float64, rot float64) layoutComp {
		r := rot
		return layoutComp{
			ID: fmt.Sprintf("%s@%.0f,%.0f", net, x, y), ComponentType: t, Net: net,
			X: x, Y: y, AnchorAvailable: true, Rotation: &r,
			BBox: &layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY},
		}
	}
	part := func(des string, minX, minY, maxX, maxY float64) layoutComp {
		return layoutComp{
			ID: des, Designator: des, ComponentType: "part",
			X: (minX + maxX) / 2, Y: (minY + maxY) / 2, AnchorAvailable: true,
			BBox: &layoutBBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY},
		}
	}
	return []layoutComp{
		{ID: "sheet", ComponentType: "sheet", BBox: &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}},
		part("U2", 684.5, 209.5, 755.5, 630.5),
		part("C5", 336.5, 649.5, 353.5, 670.5),
		part("C6", 971.5, 169.5, 988.5, 190.5),
		part("R3", 340.5, 169.5, 349.5, 190.5),
		part("R4", 414.5, 655.5, 435.5, 664.5),
		part("C4", 971.5, 649.5, 988.5, 670.5),
		part("SW1", 379.5, 297.5, 420.5, 307.5),
		part("SW2", 514.5, 297.5, 555.5, 307.5),
		// U2 的 marker —— 名字带正是它们真正占地的部分。
		f("netport", "MCU_RX", 570, 415, 534.5, 409.5, 565.5, 420.5, 0),
		f("netport", "MCU_EN", 645, 425, 609.5, 419.5, 640.5, 430.5, 0),
		f("netport", "MCU_TX", 650, 405, 614.5, 399.5, 645.5, 410.5, 0),
		f("netport", "MCU_IO0", 790, 620, 794.5, 614.5, 825.5, 625.5, 0),
		f("netflag", "+3V3", 655, 435, 644.5, 429.5, 650.5, 440.5, 90),
		f("netflag", "GND", 799.5, 245, 794.5, 249.5, 804.5, 270.5, 0),
	}
}

// ledBlockPlan 用生产路径把 LED 块算成 offsets/half(与 planBlockApply 同源)。
func ledBlockPlan(t *testing.T) (map[string]bapRoleOffset, func(string) float64, blocks.Block) {
	t.Helper()
	b, ok, err := blocks.Get("block.led_indicator_gpio")
	if err != nil || !ok {
		t.Fatalf("内嵌块库里没有 led_indicator_gpio: %v", err)
	}
	roles := make([]string, 0, len(b.Parts))
	for r := range b.Parts {
		roles = append(roles, r)
	}
	spacing := bapGridSpacing(roles, b)
	halfOf := make(map[string]float64, len(roles))
	for _, r := range roles {
		halfOf[r] = bapRoleHalfExtent(b.Parts[r].Part)
	}
	layout, lerr := b.SchematicLayout()
	if lerr != nil {
		t.Fatalf("led 块 schematic_layout 解析失败: %v", lerr)
	}
	return bapRoleOffsets(roles, layout, spacing, 4, halfOf),
		func(role string) float64 {
			if p, ok := b.Parts[role]; ok {
				if h := bapRoleHalfExtent(p.Part); h > bapPartMargin {
					return h
				}
			}
			return float64(bapPartMargin)
		}, b
}

// 障碍表必须是**全图元**口径:marker 的判定盒(含名字带)也是障碍。
// 负对照就是老口径 —— 只喂 part,U2 的左沿看上去在 684,而真值是 490。
func TestSchBlockObstacleBoxes_IncludesMarkerTextBand(t *testing.T) {
	comps := wroomPageComps()
	boxes := schBlockObstacleBoxes(comps)
	if len(boxes) != len(comps)-1 { // 图纸边框不算障碍
		t.Fatalf("除图纸边框外每个图元都该入表: got %d, comps %d", len(boxes), len(comps))
	}
	leftmostMarker := 1e9
	for _, c := range comps {
		if c.ComponentType != "netport" && c.ComponentType != "netflag" {
			continue
		}
		b := markerJudgeBBox(c)
		if b.MinX < leftmostMarker {
			leftmostMarker = b.MinX
		}
	}
	if leftmostMarker > 500 {
		t.Errorf("marker 判定盒必须含名字带(MCU_RX 实测探到 x≈490): 最左 %.1f", leftmostMarker)
	}
	// 老口径(只 part)看不见这一段 —— 这正是缺陷的入口。
	kept, _ := filterLayoutComps(comps, false)
	partLeft := 1e9
	for _, c := range kept {
		if c.Designator == "U2" && c.BBox.MinX < partLeft {
			partLeft = c.BBox.MinX
		}
	}
	if partLeft <= leftmostMarker {
		t.Fatalf("前提变了:U2 本体左沿(%.1f)本应远在 marker 左沿(%.1f)右侧", partLeft, leftmostMarker)
	}
}

// 核心验收:同样的落块顺序(先 WROOM,再 LED),LED 块的落点不许再落进 U2 的
// marker 晕圈里。判据 = 落地后本块的**晕圈矩形**与页面全图元零重叠(与
// `sch clusters` 同一把尺)。
func TestBapResolveOrigin_LEDBlockClearsWroomMarkerHalo(t *testing.T) {
	offsets, half, blk := ledBlockPlan(t)
	comps := wroomPageComps()
	sheet := &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	in := bapInput{
		Block: blk, Instance: "led1",
		OriginX: 400, OriginY: 300, // CLI 默认原点
		Sheet:     sheet,
		Obstacles: schBlockObstacleBoxes(comps),
		Bind:      map[string]string{"CTRL": "LED_GPIO"}, // 真机 E2E 用的绑定
	}
	x, y, origin, warns := bapResolveOrigin(in, offsets, half)

	halo := bapMarkerHalo(blk, in.Instance, in.Bind)
	env := bapGrow(bapBlockRect(x, y, offsets, half), halo)
	for _, o := range in.Obstacles {
		if boxesGapOverlap(env, o, bapObstacleGap) {
			t.Fatalf("落点 (%.0f,%.0f) 的晕圈仍压在已有图元上: env=%+v obstacle=%+v (relocated=%v warns=%v)",
				x, y, env, o, origin.Relocated, warns)
		}
	}
	// 出图纸是另一条硬约束,顺带钉住。
	if !boxInside(bapBlockRect(x, y, offsets, half), schUsableArea(*sheet)) {
		t.Errorf("落点让块出了图纸可用区: (%.0f,%.0f)", x, y)
	}
	t.Logf("LED 块落点 (%.0f,%.0f) halo=%.0f relocated=%v", x, y, halo, origin.Relocated)
}

// 负对照:老口径(只 part 障碍 + 无晕圈)必须**还原出那个 bug** —— 否则上面那条
// 用例可能只是运气好,而不是尺子变对了。
func TestBapResolveOrigin_BodyOnlyRulerReproducesTheOverlap(t *testing.T) {
	offsets, half, blk := ledBlockPlan(t)
	comps := wroomPageComps()
	kept, _ := filterLayoutComps(comps, false)
	var partsOnly []layoutBBox
	for _, c := range kept {
		if c.BBox != nil {
			partsOnly = append(partsOnly, *c.BBox)
		}
	}
	in := bapInput{
		Block: blk, Instance: "led1",
		OriginX: 400, OriginY: 300,
		Sheet:     &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825},
		Obstacles: partsOnly,
		// Bind 留空:老路径的 halo 无从谈起,这里只复现"本体尺"的判断
	}
	// 老尺子 = 本体矩形直接判碰(bapMarkerHalo 之前的 collides)。
	rect := bapBlockRect(400, 450, offsets, half)
	for _, o := range partsOnly {
		if boxesGapOverlap(rect, o, bapObstacleGap) {
			t.Fatalf("前提变了:本体尺在 (400,450) 本应判定「没撞」(真机就是这样落的): %+v", o)
		}
	}
	_ = in
	// 而全图元尺子在同一个点必须判"撞" —— 两把尺的分歧就是缺陷本身。
	halo := bapMarkerHalo(blk, "led1", map[string]string{"CTRL": "LED_GPIO"})
	env := bapGrow(rect, halo)
	hit := false
	for _, o := range schBlockObstacleBoxes(comps) {
		if boxesGapOverlap(env, o, bapObstacleGap) {
			hit = true
			break
		}
	}
	if !hit {
		t.Fatal("全图元尺子必须在 (400,450) 判定重叠,否则新判据抓不到真机那个缺陷")
	}
}

// 晕圈半径的口径必须与 autoconnect 同源(OffsetMin + netport 总占地),且随最长
// 网名增长 —— 写死一个数就会在长网名的块上原样复发。
func TestBapMarkerHalo_TracksLongestNetName(t *testing.T) {
	b := blocks.Block{Ports: map[string]blocks.Port{
		"CTRL": {DefaultNet: "LED_CTRL"},
		"GND":  {DefaultNet: "GND"},
	}}
	base := bapMarkerHalo(b, "led1", nil)
	want := defaultAutoconnectRules().OffsetMin + acPortTotalLen("LED_CTRL")
	if base != want {
		t.Errorf("halo 应 = OffsetMin + acPortTotalLen(最长网名): got %v want %v", base, want)
	}
	long := bapMarkerHalo(b, "led1", map[string]string{"CTRL": "USB_HOST_DP_TERMINATION"})
	if long <= base {
		t.Errorf("--bind 出更长的网名时晕圈必须变大: %v vs %v", long, base)
	}
	// 短名不许把晕圈缩到不合理:内部网名走 <INSTANCE>_N<k>,有下限。
	short := bapMarkerHalo(blocks.Block{Ports: map[string]blocks.Port{"G": {DefaultNet: "5V"}}}, "c1", nil)
	if short < defaultAutoconnectRules().OffsetMin+acPortTotalLen("NNNNNNNN") {
		t.Errorf("短网名也要按下限(8 字符)算: %v", short)
	}
}

// 页面挤到晕圈无论如何放不下时,**不许**回落到请求原点压在别人本体上 ——
// 降级重试必须仍然给出一个本体安全的落点,并说清楚晕圈没留住。
func TestBapResolveOrigin_DegradesToBodyRulerWhenPageIsTight(t *testing.T) {
	offsets, half, blk := ledBlockPlan(t)
	sheet := &layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825}
	// 一条横贯全页的障碍带,只在下方留出刚好够本体、不够晕圈的一条缝。
	var obstacles []layoutBBox
	obstacles = append(obstacles,
		layoutBBox{MinX: 0, MinY: 260, MaxX: 1170, MaxY: 825},
		layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 60})
	in := bapInput{
		Block: blk, Instance: "led1",
		OriginX: 400, OriginY: 300,
		Sheet:     sheet,
		Obstacles: obstacles,
	}
	x, y, origin, warns := bapResolveOrigin(in, offsets, half)
	rect := bapBlockRect(x, y, offsets, half)
	for _, o := range obstacles {
		if boxesGapOverlap(rect, o, bapObstacleGap) {
			t.Fatalf("降级重试后本体仍压在障碍上: rect=%+v obstacle=%+v (warns=%v)", rect, o, warns)
		}
	}
	if !origin.Relocated {
		t.Errorf("动过坐标就必须标 Relocated: %+v", origin)
	}
	if len(warns) == 0 {
		t.Error("晕圈没留住必须说出来,否则用户不知道要去跑 sch clusters")
	}
}
