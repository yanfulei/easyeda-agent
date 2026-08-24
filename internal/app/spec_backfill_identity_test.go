package app

// spec_backfill_identity_test.go — 同名重建的死页不再进回填分母。
//
// 用的是**真实的脏状态文件**(testdata/workflow/dirty-ceshi.json,原样拷自
// ~/.easyeda-agent/workflow/ceshi.json:7 页里 4 页属于已删除的同名工程)。
// 合成 fixture 证明不了「旧格式读得出来」,真文件能。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// 真实脏文件里的页:三页属于当时活着的 ceshi,四页是前几次同名重建的残留。
var (
	dirtyLivePages = []string{"9b7682d93be74820", "8716c3fc85f9a474", "f457006afa37c6d3"}
	dirtyDeadPages = []string{"64c23f12421fa656", "73833c6370bb5846", "4b117ef43af42bf3", "1b75a705f6fea3d6"}
)

// loadDirtyCeshi 把真实脏文件放进一个临时 workflow 目录后加载。
func loadDirtyCeshi(t *testing.T) *workflow.State {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(workflow.EnvDir, dir)
	raw, err := os.ReadFile(filepath.Join("testdata", "workflow", "dirty-ceshi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ceshi.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := workflow.Load("ceshi")
	if err != nil {
		t.Fatalf("真实旧格式脏文件必须读得出来: %v", err)
	}
	return st
}

// ① 旧格式能读,而且**行为不变**:没有活体 uuid 时一页都不收窄(升级本身不许
// 改变任何现有工程的回填结果)。
func TestDirtyCeshiLoadsAndDefaultsToNoNarrowing(t *testing.T) {
	st := loadDirtyCeshi(t)
	if len(st.GroupsByPage) != 7 {
		t.Fatalf("脏文件应有 7 页组记账,实得 %d", len(st.GroupsByPage))
	}
	if st.ProjectUUID != "" || len(st.PageOwners) != 0 {
		t.Fatal("旧格式文件不该凭空长出身份")
	}
	groups, skipped := specCollectGroups(st, "")
	if len(skipped) != 0 {
		t.Fatalf("没有活体 uuid 时不该收窄,却跳过了 %v", skipped)
	}
	if len(groups) == 0 {
		t.Fatal("跨页收集空了")
	}
	// 分母确实被污染:同一个块在多页各有一份来源。
	byBlock := map[string]int{}
	for _, g := range groups {
		byBlock[g.BlockID]++
	}
	if byBlock["sy8089_buck_3v3"] < 3 {
		t.Fatalf("这份脏文件本该让 sy8089_buck_3v3 出现在 3 页上,实得 %d", byBlock["sy8089_buck_3v3"])
	}
}

// ② 拿活体页表核销一次之后,四页死页**不再参与**跨页匹配 —— 本次要根治的症状。
func TestDirtyCeshiDeadPagesLeaveTheBackfillDenominator(t *testing.T) {
	st := loadDirtyCeshi(t)
	const liveUUID = "live-project-uuid"
	st.Bind(liveUUID)

	stamped, foreign, refused := st.AdoptLivePages(liveUUID, dirtyLivePages)
	if refused {
		t.Fatal("给了活体页表却被拒绝")
	}
	if len(stamped) != len(dirtyLivePages) {
		t.Fatalf("活页应全部盖戳,实得 %v", stamped)
	}
	sort.Strings(foreign)
	want := append([]string(nil), dirtyDeadPages...)
	sort.Strings(want)
	if strings.Join(foreign, ",") != strings.Join(want, ",") {
		t.Fatalf("外来页判定错了:\n got %v\nwant %v", foreign, want)
	}

	groups, skipped := specCollectGroups(st, st.ScopeUUID(liveUUID))
	sort.Strings(skipped)
	// 4 页死页里有组记账的都必须被跳过并**报出来**(静默少收与静默多收一样坏)。
	for _, dead := range dirtyDeadPages {
		if len(st.GroupsByPage[dead]) == 0 {
			continue
		}
		found := false
		for _, s := range skipped {
			if s == dead {
				found = true
			}
		}
		if !found {
			t.Fatalf("死页 %s 仍在回填分母里(skipped=%v)", dead, skipped)
		}
	}
	byBlock := map[string]int{}
	for _, g := range groups {
		byBlock[g.BlockID]++
	}
	if byBlock["sy8089_buck_3v3"] != 1 {
		t.Fatalf("收窄后 sy8089_buck_3v3 应只剩活页那一份,实得 %d", byBlock["sy8089_buck_3v3"])
	}
	// 收窄只影响参与,不删数据。
	if len(st.GroupsByPage) != 7 {
		t.Fatalf("收窄不该删任何页,实剩 %d", len(st.GroupsByPage))
	}
}

// ③ 收窄后的回填结果必须只反映活页。用真实脏文件 + 一份声明了三个块的 spec 跑
// 端到端:模块的 parts 只能来自活页的组。
func TestDirtyCeshiBackfillUsesOnlyLivePages(t *testing.T) {
	st := loadDirtyCeshi(t)
	const liveUUID = "live-project-uuid"
	st.Bind(liveUUID)
	if _, _, refused := st.AdoptLivePages(liveUUID, dirtyLivePages); refused {
		t.Fatal("reap refused")
	}
	// 把死页的一个组改脏:同名重建之后位号完全可能不同,而 union 会把两边并起来。
	st.GroupsByPage["1b75a705f6fea3d6"] = []*workflow.Group{{
		ID: "gx", Name: "sy8089_buck_3v3(C1)", BlockID: "block.sy8089_buck_3v3",
		Members: []string{"C99", "R99"},
	}}
	if err := workflow.Save(st); err != nil {
		t.Fatal(err)
	}

	specPath := filepath.Join(t.TempDir(), "s0.json")
	if err := os.WriteFile(specPath, []byte(`{
  "project": "ceshi",
  "modules": [{"name": "power", "block": "sy8089_buck_3v3", "parts": []}]
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, patched, err := runSpecBackfill(specPath, "ceshi", liveUUID)
	if err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	var got spec.Spec
	if err := json.Unmarshal(patched, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Modules) != 1 {
		t.Fatalf("modules = %d", len(got.Modules))
	}
	parts := strings.Join(got.Modules[0].PartsOf(), ",")
	if strings.Contains(parts, "C99") || strings.Contains(parts, "R99") {
		t.Fatalf("死页的位号被写进了 spec:%s", parts)
	}
	if !strings.Contains(parts, "U1") {
		t.Fatalf("活页的位号丢了:%s", parts)
	}
	// 跳过的页必须出现在警告里 —— 判据静默收窄和判据静默污染一样不可接受。
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "属于另一个工程") {
		t.Fatalf("收窄没有被报出来,warnings=%v", res.Warnings)
	}
}

// ④ 另一个工程的状态文件不受影响:同一个目录里跑完 ceshi 的核销/清理,
// 别的工程文件必须逐字节不变(用户的真实工程 BBClaw-AI 等都在同一个目录)。
func TestOtherProjectStateUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(workflow.EnvDir, dir)
	raw, err := os.ReadFile(filepath.Join("testdata", "workflow", "dirty-ceshi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ceshi.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	other := `{"project":"BBClaw-AI","confirmed":{"imported":true},` +
		`"groupsByPage":{"zzz111":[{"id":"g1","members":["R1"]}]},"updatedAt":"2026-07-30T18:19:00+08:00"}`
	otherPath := filepath.Join(dir, "BBClaw-AI.json")
	if err := os.WriteFile(otherPath, []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := workflow.Load("ceshi")
	if err != nil {
		t.Fatal(err)
	}
	st.Bind("live-project-uuid")
	st.AdoptLivePages("live-project-uuid", dirtyLivePages)
	st.PrunePages(st.ForeignPages("live-project-uuid"))
	if err := workflow.Save(st); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != other {
		t.Fatal("另一个工程的状态文件被动了")
	}
	// 顺带确认清理确实只清了死页。
	cleaned, err := workflow.Load("ceshi")
	if err != nil {
		t.Fatal(err)
	}
	if len(cleaned.GroupsByPage) != len(dirtyLivePages) {
		t.Fatalf("清理后应只剩 %d 页,实剩 %d", len(dirtyLivePages), len(cleaned.GroupsByPage))
	}
	for _, p := range dirtyLivePages {
		if len(cleaned.GroupsByPage[p]) == 0 {
			t.Fatalf("活页 %s 被误删", p)
		}
	}
}
