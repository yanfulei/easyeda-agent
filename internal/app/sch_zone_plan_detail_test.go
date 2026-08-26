package app

import (
	"strings"
	"testing"
)

// 回归:labelCollisions 只报一个数,不说是谁压了谁。
//
// 2026-08-26 esp32MiniRequire 实测,MCU_CORE 页卡在这一项上:
//
//	{"sheetOverflow":0,"partitionOverlap":0,"titleBlockHits":0,
//	 "moduleOutsideZone":0,"labelCollisions":1,"sheetMarginHits":0}
//
// `zone-draw` 因它拒绝画框,而同一份输出里 partitionOverlap / titleBlockHits /
// moduleOutsideZone 都带 Detail(点名是谁、差多少、给一条能抄的命令),
// 唯独它光秃秃一个 1 —— 只能靠反复改字号试。拦得住却不说拦在哪,等于没拦。
func TestValidatePartitions_LabelCollisionHasAttribution(t *testing.T) {
	// 一个区,标题带在框顶;模块本体伸进标题带里。
	rects := []partitionRect{{
		Modules:   []string{"MCU_ESP32S3"},
		BBox:      layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 300},
		TitleBBox: layoutBBox{MinX: 0, MinY: 270, MaxX: 400, MaxY: 300},
	}}
	mods := []partitionModule{{
		Name: "MCU_ESP32S3",
		BBox: layoutBBox{MinX: 50, MinY: 100, MaxX: 350, MaxY: 285}, // 顶端探进标题带 15
	}}
	plan := partitionPlan{
		Sheet:      layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825},
		Partitions: rects,
	}

	v := validatePartitions(plan, mods, nil)
	if v.LabelCollisions == 0 {
		t.Fatal("用例构造错了:标题带确实压住了模块本体,该报 labelCollisions")
	}
	if len(v.LabelCollisionDetail) != v.LabelCollisions {
		t.Fatalf("labelCollisions=%d 却只有 %d 条归因 —— 报数不报因,与其余五项不同口径",
			v.LabelCollisions, len(v.LabelCollisionDetail))
	}
	d := strings.Join(v.LabelCollisionDetail, "\n")
	// 归因必须点名**是谁**,并给出一条能直接抄去跑的下一步(本仓的复发病:
	// 判据只报数、不给出路)。
	for _, kw := range []string{"MCU_ESP32S3", "sch group-move", "font-size"} {
		if !strings.Contains(d, kw) {
			t.Fatalf("归因缺少 %q —— 读的人无法定位或无法执行:\n%s", kw, d)
		}
	}
}

// 不压就不报 —— 收紧不许制造噪音。
func TestValidatePartitions_NoLabelCollisionStaysSilent(t *testing.T) {
	rects := []partitionRect{{
		Modules:   []string{"MCU"},
		BBox:      layoutBBox{MinX: 0, MinY: 0, MaxX: 400, MaxY: 300},
		TitleBBox: layoutBBox{MinX: 0, MinY: 270, MaxX: 400, MaxY: 300},
	}}
	mods := []partitionModule{{
		Name: "MCU",
		BBox: layoutBBox{MinX: 50, MinY: 100, MaxX: 350, MaxY: 250}, // 离标题带还有 20
	}}
	plan := partitionPlan{
		Sheet:      layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825},
		Partitions: rects,
	}

	v := validatePartitions(plan, mods, nil)
	if v.LabelCollisions != 0 || len(v.LabelCollisionDetail) != 0 {
		t.Fatalf("没压却报了:%d 条 %v", v.LabelCollisions, v.LabelCollisionDetail)
	}
}
