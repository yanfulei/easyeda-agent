package app

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// 回归:同一个工程内「页被删掉重建」留下的幽灵组,会把回填写成一份画布上不存在的位号表。
//
// 2026-08-26 esp32MiniRequire 端到端实测:`ceshi` 工程被重建过,
// ~/.easyeda-agent/workflow/ceshi.json 里留着 5 个**已删除页**的虚拟组。
// 此时只落了 ams1117 一个块(4 件),回填却把另外两个还没动过的模块改写了:
//
//	spec ✓ MCU.parts:U2 C4 C5 R1 C6 R2 → C1 C10 C11 C12 C2 C3 R1 R10 R11 R2 U1 U4
//
// 画布上当时总共只有 4 个器件。
//
// PageInScope 拦不住这一类:它按 **projectUuid** 收窄,而这些死页的 owner
// 就是当前工程本身 —— 同工程内的删页重建对它完全透明。
func TestSpecPlanBackfill_GhostInstancesOfSameBlockAreAmbiguous(t *testing.T) {
	src := `{"modules":[
	  {"name":"MCU","kind":"MCU","block":"block.esp32s3_wroom1_module","parts":["U2","C4","C5","R1","C6","R2"]}
	]}`
	s, err := spec.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	// 两个**不同实例**的同名块:一个来自已删除的旧页(instance C1),
	// 一个来自另一张已删除的旧页(instance esp32_wifi)。
	groups := []specGroupSource{
		{Label: "esp32s3_wroom1_module(C1)/U_3V3", Name: "esp32s3_wroom1_module(C1)/U_3V3",
			BlockID: "esp32s3_wroom1_module", Instance: "C1", Members: []string{"C1", "C3"}},
		{Label: "esp32s3_wroom1_module(C1)/U_EN", Name: "esp32s3_wroom1_module(C1)/U_EN",
			BlockID: "esp32s3_wroom1_module", Instance: "C1", Members: []string{"C2", "R1"}},
		{Label: "esp32s3_wroom1_module(esp32_wifi)/U_3V3", Name: "esp32s3_wroom1_module(esp32_wifi)/U_3V3",
			BlockID: "esp32s3_wroom1_module", Instance: "esp32_wifi", Members: []string{"C10", "C12"}},
		{Label: "esp32s3_wroom1_module(esp32_wifi)/U_EN", Name: "esp32s3_wroom1_module(esp32_wifi)/U_EN",
			BlockID: "esp32s3_wroom1_module", Instance: "esp32_wifi", Members: []string{"C11", "R10"}},
	}
	want, res := specPlanBackfill(s, groups)

	// 硬判据:两个实例分不清哪个是活的 —— 绝不能把它们并起来写进文件。
	if len(want) != 0 {
		t.Fatalf("把两个块实例的位号并起来写了:%v", want["MCU"])
	}
	if len(res.Changes) != 0 {
		t.Fatalf("changes 应为空,实际:%+v", res.Changes)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("跳过了却不报警 —— 静默少写与静默写错一样坏")
	}
	joined := strings.Join(res.Warnings, "\n")
	for _, kw := range []string{"MCU", "C1", "esp32_wifi"} {
		if !strings.Contains(joined, kw) {
			t.Fatalf("告警没点名 %q,读的人无法定位:\n%s", kw, joined)
		}
	}
}

// 反向对照:同一个实例拆成多个功能子群,**必须**照常合并(这是 block-apply 的正常形态)。
// 上面的收紧不能误伤它。
func TestSpecPlanBackfill_SubgroupsOfOneInstanceStillMerge(t *testing.T) {
	src := `{"modules":[
	  {"name":"MCU","kind":"MCU","block":"block.esp32s3_wroom1_module","parts":["OLD1"]}
	]}`
	s, err := spec.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	groups := []specGroupSource{
		{Label: "esp32s3_wroom1_module(C4)/U_3V3", Name: "esp32s3_wroom1_module(C4)/U_3V3",
			BlockID: "esp32s3_wroom1_module", Instance: "C4", Members: []string{"C4", "C6"}},
		{Label: "esp32s3_wroom1_module(C4)/U_EN", Name: "esp32s3_wroom1_module(C4)/U_EN",
			BlockID: "esp32s3_wroom1_module", Instance: "C4", Members: []string{"C5", "R1"}},
		{Label: "esp32s3_wroom1_module(C4)", Name: "esp32s3_wroom1_module(C4)",
			BlockID: "esp32s3_wroom1_module", Instance: "C4", Members: []string{"U2"}},
	}
	want, res := specPlanBackfill(s, groups)
	if got := strings.Join(want["MCU"], ","); got != "C4,C5,C6,R1,U2" {
		t.Fatalf("同一实例的子群没合并:%q (warnings=%v)", got, res.Warnings)
	}
}

// 老状态(记账里没写 Instance)不能因为这条收紧而一夜作废 —— 空 Instance 视作同一实例。
func TestSpecPlanBackfill_LegacyGroupsWithoutInstanceStillMerge(t *testing.T) {
	src := `{"modules":[
	  {"name":"MCU","kind":"MCU","block":"block.esp32s3_wroom1_module","parts":["OLD1"]}
	]}`
	s, _ := spec.Parse([]byte(src))
	groups := []specGroupSource{
		{Label: "g1", Name: "esp32s3_wroom1_module", BlockID: "esp32s3_wroom1_module", Members: []string{"U3", "C11"}},
		{Label: "g2", Name: "esp32s3_wroom1_module/U_3V3", BlockID: "esp32s3_wroom1_module", Members: []string{"C12"}},
	}
	want, res := specPlanBackfill(s, groups)
	if got := strings.Join(want["MCU"], ","); got != "C11,C12,U3" {
		t.Fatalf("无 Instance 的老组被误判成歧义:%q (warnings=%v)", got, res.Warnings)
	}
}

// specCollectGroups 侧:给了活页集合就必须把不在其中的页整页丢掉,并如实报出来。
// 这是「同工程内删页重建」的正解 —— PageInScope 的 projectUuid 收窄对它无效。
func TestSpecCollectGroups_DropsPagesAbsentFromLiveSet(t *testing.T) {
	st := &workflow.State{
		GroupsByPage: map[string][]*workflow.Group{
			// 活页
			"22ce0215a9d39a42": {{ID: "g1", Name: "LDO_3V3", BlockID: "block.ams1117_ldo_3v3",
				Instance: "C1", Members: []string{"U1", "C1"}}},
			// 已删除的旧页(同一工程,PageInScope 放行)
			"02a8ba989be213d9": {{ID: "g1", Name: "esp32s3_wroom1_module(C1)/U_3V3",
				BlockID: "block.esp32s3_wroom1_module", Instance: "C1", Members: []string{"C1", "C3"}}},
			"fee77ee900c492ff": {{ID: "g1", Name: "esp32s3_wroom1_module(esp32_wifi)/U_3V3",
				BlockID: "block.esp32s3_wroom1_module", Instance: "esp32_wifi", Members: []string{"C10", "C12"}}},
		},
	}
	live := map[string]bool{"22ce0215a9d39a42": true}

	groups, skipped := specCollectGroupsLive(st, "", live)
	if len(groups) != 1 || groups[0].Instance != "C1" || groups[0].BlockID != "ams1117_ldo_3v3" {
		t.Fatalf("活页之外的组没被丢掉:%+v", groups)
	}
	if len(skipped) != 2 {
		t.Fatalf("丢掉的页必须报出来,skipped=%v", skipped)
	}
}

// 拿不到活页集合(离线、连不上编辑器)时行为必须与收紧前完全一致 ——
// backfill 的离线契约不能因为这条修复而破掉。
func TestSpecCollectGroups_NilLiveSetKeepsOfflineBehaviour(t *testing.T) {
	st := &workflow.State{
		GroupsByPage: map[string][]*workflow.Group{
			"22ce0215a9d39a42": {{ID: "g1", Name: "A", BlockID: "block.x", Members: []string{"U1"}}},
			"02a8ba989be213d9": {{ID: "g1", Name: "B", BlockID: "block.y", Members: []string{"U2"}}},
		},
	}
	groups, skipped := specCollectGroupsLive(st, "", nil)
	if len(groups) != 2 {
		t.Fatalf("nil 活页集合应等价于不收窄,实际收到 %d 组", len(groups))
	}
	if len(skipped) != 0 {
		t.Fatalf("没收窄却报了 skipped:%v", skipped)
	}
}
