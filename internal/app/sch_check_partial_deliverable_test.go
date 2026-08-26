package app

import (
	"strings"
	"testing"
)

// 回归:分区框 / 电路说明的判据是「有没有」而不是「够不够」,于是**漏了一部分**查不出来。
//
// 2026-08-26 esp32MiniRequire 端到端实测两处:
//
//	MCU_CORE 页 6 个模块,zone-draw 只画上 2 个框(4 个报 text create returned
//	undefined),`sch check` 报 0 findings —— 因为 frameRects != 0;
//	同页 3 个模块只写成 2 条说明(第 3 条命令静默失败),同样 0 findings ——
//	因为 notes != 0。
//
// 用户当场问「zone框选所有元素都不缺了吗」,而 check 答不上来:它只会回答
// 「一个都没有吗」。缺 4 个框、缺 1 条说明,与全都有,在它眼里是同一个答案。
func TestPartitionFinding_PartialFramesAreReported(t *testing.T) {
	// 6 个模块只画上 2 个框 —— 事故现场的原始数据。
	got := partitionFindingForZones(10, 2, 2, 5, 6)
	var found *checkFinding
	for _, f := range got {
		if f.Type == "missing-partition" {
			found = f
		}
	}
	if found == nil {
		t.Fatalf("6 个模块只画 2 个框却不报,findings=%+v", got)
	}
	// 报文必须带够「差几个」,否则读的人不知道要补什么。
	for _, kw := range []string{"2", "6"} {
		if !strings.Contains(found.Message, kw) {
			t.Fatalf("报文没写清 %s 个框 / %s 个模块:%s", kw, kw, found.Message)
		}
	}
}

func TestPartitionFinding_PartialNotesAreReported(t *testing.T) {
	// 3 个模块、3 个框、只有 2 条说明(第 3 条写失败了)。
	// textCount = 3 区名 + 2 说明 = 5;labelTexts = 3。
	got := partitionFindingForZones(10, 3, 3, 5, 3)
	var found *checkFinding
	for _, f := range got {
		if f.Type == "missing-note" {
			found = f
		}
	}
	if found == nil {
		t.Fatalf("3 个模块只有 2 条说明却不报,findings=%+v", got)
	}
	if !strings.Contains(found.Message, "2") || !strings.Contains(found.Message, "3") {
		t.Fatalf("报文没写清 2 条 / 3 个模块:%s", found.Message)
	}
}

// 齐了就不能报 —— 收紧不许制造噪音。
func TestPartitionFinding_CompleteDeliverablesStaySilent(t *testing.T) {
	// 3 个模块 / 3 个框 / 3 条说明(textCount = 3 区名 + 3 说明)。
	if got := partitionFindingForZones(10, 3, 3, 6, 3); len(got) != 0 {
		t.Fatalf("交付件齐全却报了:%+v", got)
	}
	// 说明比模块多(一个模块写了两段)也算齐。
	if got := partitionFindingForZones(10, 3, 3, 8, 3); len(got) != 0 {
		t.Fatalf("说明多于模块数却报了:%+v", got)
	}
}

// 没有模块记账(zones=0)时退回老口径:证不出「该有几个」就不能判「缺了几个」。
func TestPartitionFinding_NoZoneAccountingKeepsOldBehaviour(t *testing.T) {
	// 一个框都没有 → 照报(老口径)。
	if got := partitionFindingForZones(10, 0, 0, 0, 0); len(got) != 2 {
		t.Fatalf("zones=0 且啥都没有,应报 missing-partition + missing-note,实际:%+v", got)
	}
	// 有框有说明 → 不报(证不出缺几个,不许瞎猜)。
	if got := partitionFindingForZones(10, 1, 1, 3, 0); len(got) != 0 {
		t.Fatalf("zones=0 且有框有说明却报了:%+v", got)
	}
}

// 器件数低于门槛的小页不参与(与老口径一致)。
func TestPartitionFinding_SmallPageExempt(t *testing.T) {
	if got := partitionFindingForZones(schPartitionMinParts-1, 0, 0, 0, 5); len(got) != 0 {
		t.Fatalf("小页不该被判:%+v", got)
	}
}
