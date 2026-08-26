package app

import (
	"strings"
	"testing"
)

// 回归:R5 的重叠判据用了**另一把尺**,擦边接触就把整页拒了。
//
// 2026-08-26 esp32MiniRequire 实测,POWER 页 J2(KF301-2P 螺钉端子,两脚同侧、
// y 差 10)反复被 phase A 拒绝,整页停手且不给出路:
//
//	phase A(PWR_INPUT): J2: 端子重叠 VIN_EXT(left) × GND(left) —— R5 硬不变式(自短路防线)
//
// 而画布上那两支旗 `sch check` 的 marker-overlap **一条都不报** —— 因为
// zfCheckTermOverlap 用的是裸 boxesOverlap(碰到就算),而同一个文件里的
// zfMarkerCollides(与 sch check 的 marker-overlap 逐字同源)用的是
// overlapExtent + schMarkerOverlapEps 噪声地板。
//
// 同一个文件里的注释自己写着「规划器让位的门槛必须就是判据会报的那条线」,
// 而 R5 这处没照做 —— 典型的「两把尺」复发病:低于噪声地板的擦边,判据不报、
// 规划器却当场判死。
func TestZfCheckTermOverlap_UsesTheSameRulerAsCheck(t *testing.T) {
	// 擦边:重叠 1.0,正好等于 schMarkerOverlapEps 噪声地板 —— sch check 不报,
	// 规划器也不该报。
	grazing := zfPlacedGroup{Designator: "J2", Terms: []zfPlacedTerm{
		{Kind: "netflag", Net: "VIN_EXT", Dir: "left",
			BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 20, MaxY: 20}},
		{Kind: "netflag", Net: "GND", Dir: "left",
			BBox: layoutBBox{MinX: 19, MinY: 0, MaxX: 39, MaxY: 20}},
	}}
	if err := zfCheckTermOverlap(grazing); err != nil {
		t.Fatalf("擦边(重叠 1.0 = 噪声地板)被判自短路,而 sch check 根本不报它:%v", err)
	}
	// 判据必须与 zfMarkerCollides 一致 —— 一个概念一把尺。
	a, b := grazing.Terms[0].BBox, grazing.Terms[1].BBox
	if zfMarkerCollides(a, b) {
		t.Fatal("用例构造错了:这两个框在 marker-overlap 的尺子下本就不算撞")
	}

	// 真撞:重叠远超噪声地板 —— 两把尺都必须报。
	real := zfPlacedGroup{Designator: "X", Terms: []zfPlacedTerm{
		{Kind: "netflag", Net: "A", Dir: "down",
			BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 20, MaxY: 20}},
		{Kind: "netflag", Net: "B", Dir: "down",
			BBox: layoutBBox{MinX: 10, MinY: 10, MaxX: 30, MaxY: 30}},
	}}
	err := zfCheckTermOverlap(real)
	if err == nil {
		t.Fatal("真重叠必须报 R5 违例")
	}
	if !zfMarkerCollides(real.Terms[0].BBox, real.Terms[1].BBox) {
		t.Fatal("用例构造错了:这两个框该被 marker-overlap 判为撞")
	}
	// 拒绝必须给得出下一步 —— 这是本仓的复发病之一(判据不给出路)。
	for _, kw := range []string{"A", "B"} {
		if !strings.Contains(err.Error(), kw) {
			t.Fatalf("报文没点名是哪两支端子:%s", err.Error())
		}
	}
}

// R5 的检查集必须 == 让位的参与集。
//
// 真机诊断(2026-08-26 POWER 页 J2 = KF301-5.0-2P)给出的确切几何:
//
//	J2 本体 21×31 multiPin=false
//	  netport  VIN_EXT  side=left  pin=(-9.5,20.5) hasPin=true
//	  netflag  GND      side=left  pin=(-9.5,10.5) hasPin=true
//
// 侧面的 **netport 不参与让位**(首版就定的规则:port 恒水平、短桩不进梯次,
// 把它拉进来会把区框撑爆),所以它也不进 placedBySide —— 同侧的 netflag 以为
// 左边没人,用默认桩长放下,正好压在 port 标签上。而 R5 过去检查**所有**端子,
// 当场判死这张完全合法的拓扑:让位器不管的东西,判据拿它判死 = 拦得住却放不开。
func TestZfCheckTermOverlap_SkipsPairsTheYielderCannotFix(t *testing.T) {
	terms := []zfTerm{
		{Kind: "netport", Net: "VIN_EXT", Side: "left", PinX: -9.5, PinY: 20.5},
		{Kind: "netflag", Net: "GND", Side: "left", PinX: -9.5, PinY: 10.5},
	}
	out := zfPlacedGroup{Designator: "J2",
		Body: layoutBBox{MinX: 0, MinY: 0, MaxX: 21, MaxY: 31}}
	zfPlaceMeasuredTerms(&out, terms, func(t zfTerm) string { return t.Side })

	if err := zfCheckTermOverlap(out); err != nil {
		t.Fatalf("KF301 的 port×flag 同侧组合被 R5 判死,而让位器根本管不到 port:%v", err)
	}
	// 但真正的自短路(同向 + 桩线共线)仍必须拦住 —— 放松的只是「让位器管不到的
	// 视觉重叠」,不是自短路本身。
	if err := zfCheckPassiveOpposed(out); err != nil {
		t.Fatalf("两脚 y 差 10、桩平行,不该判共线自短路:%v", err)
	}
}

// 负对照:两支都是会让位的 netflag 时,真重叠照样判死 —— 放松不能变成放行。
func TestZfCheckTermOverlap_TwoYieldingFlagsStillChecked(t *testing.T) {
	g := zfPlacedGroup{Designator: "X", Terms: []zfPlacedTerm{
		{Kind: "netflag", Net: "A", Dir: "left",
			BBox: layoutBBox{MinX: 0, MinY: 0, MaxX: 40, MaxY: 20}},
		{Kind: "netflag", Net: "B", Dir: "left",
			BBox: layoutBBox{MinX: 10, MinY: 5, MaxX: 50, MaxY: 25}},
	}}
	if err := zfCheckTermOverlap(g); err == nil {
		t.Fatal("两支都会让位的旗真重叠,必须判死")
	}
}

// 真正该钉死的不变式:**让位循环停下来的那一刻,R5 必须放行**。
//
// 让位循环(zfPlaceMeasuredTerms)让到 zfMarkerCollides==false 就停 —— 也就是
// 允许残留 ≤ schMarkerOverlapEps 的擦边。R5 过去用裸 boxesOverlap「碰到就算」,
// 于是 `0 < 重叠 ≤ 1.0` 这条缝里的几何:让位器认为已经让好了、检查器却判死。
// J2(KF301,两脚都在左缘、y 差 10,标签比脚距高)正是掉进这条缝里。
//
// 两边用同一把尺,这条缝在结构上就不存在了 —— 这个测试就是那句话的可执行形式。
func TestZfPlaceMeasuredTerms_YieldedLayoutAlwaysPassesR5(t *testing.T) {
	// KF301-2P:两脚同在左缘、y 差 10;两支旗都朝左。
	terms := []zfTerm{
		{Kind: "netflag", Net: "VIN_EXT", Side: "left", PinX: 0, PinY: 110},
		{Kind: "netflag", Net: "GND", Side: "left", PinX: 0, PinY: 100},
	}
	out := zfPlacedGroup{Designator: "J2",
		Body: layoutBBox{MinX: 0, MinY: 80, MaxX: 60, MaxY: 130}}
	zfPlaceMeasuredTerms(&out, terms, func(t zfTerm) string { return t.Side })

	if len(out.Terms) != 2 {
		t.Fatalf("端子没放全:%+v", out.Terms)
	}
	if err := zfCheckTermOverlap(out); err != nil {
		t.Fatalf("让位器认为已经让开了,R5 却判死 —— 两把尺又分叉了:%v", err)
	}
	// 让位必须真的发生在**桩长**上(执行侧唯一可控的自由度),而不是把标签
	// 横向挪走 —— connect_pin 只有 direction + offset 两个旋钮。
	if out.Terms[0].Offset == out.Terms[1].Offset {
		t.Fatalf("两支同侧旗用了相同桩长,标签必然叠:offsets=%v/%v",
			out.Terms[0].Offset, out.Terms[1].Offset)
	}
}
