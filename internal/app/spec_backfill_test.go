package app

// spec_backfill_test.go — S0 spec 位号回填的自带测试(#181 第三份复盘第 3 条)。
//
// 最重要的一条是**外科手术**:回填只许改 modules[i].parts 那一段字节。
// memory「attrs-backfill 投影键灭位号」那次是 166 个真实位号被整包写抹成 `C?`;
// 同一条教训在这里的形式是「Unmarshal 到结构体再 Marshal 回去会静默丢掉未知字段、
// 键序与缩进」。TestSpecPatchParts_SurgicalOnly 就是钉这一条的。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// specFixture 是一份"手写风格"的 spec:带未知顶层键、未知模块键、非字母序的键序、
// 自定义缩进与注释性字段 —— 全部都是整包重写会丢掉的东西。
const specFixture = `{
  "board": "compact",
  "costTier": "basic",
  "x-owner": "mikas",
  "modules": [
    {
      "name": "MCU",
      "kind": "MCU",
      "zone": "center",
      "block": "esp32s3_wroom1_module",
      "parts": ["U1", "C1", "C2"],
      "x-note": "主控"
    },
    {
      "name": "POWER",
      "kind": "POWER",
      "zone": "left",
      "parts": ["U2"]
    }
  ],
  "flow": ["POWER", "MCU"]
}
`

func TestSpecPatchParts_SurgicalOnly(t *testing.T) {
	out, err := specPatchParts([]byte(specFixture), map[string][]string{
		"MCU": {"C11", "C12", "U3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	// 1. 改对了。
	if !strings.Contains(got, `"parts": ["C11","C12","U3"]`) {
		t.Fatalf("parts 没被替换:\n%s", got)
	}
	// 2. **未知字段一个不丢** —— 这是那次事故的核心教训。
	for _, keep := range []string{`"x-owner": "mikas"`, `"x-note": "主控"`, `"costTier": "basic"`} {
		if !strings.Contains(got, keep) {
			t.Fatalf("整包重写丢掉了 %s:\n%s", keep, got)
		}
	}
	// 3. **没被点名的模块一个字节都不许动**。
	if !strings.Contains(got, `"parts": ["U2"]`) {
		t.Fatalf("动了没被点名的 POWER 模块:\n%s", got)
	}
	// 4. **键序与缩进保持**:board 仍在最前、modules 在 costTier 之后 —— 一份手写
	//    文件被重排成字母序,diff 里就是"整个文件都变了"。
	if strings.Index(got, `"board"`) > strings.Index(got, `"costTier"`) {
		t.Fatalf("键序被重排了:\n%s", got)
	}
	if !strings.Contains(got, "\n      \"kind\": \"MCU\",") {
		t.Fatalf("缩进被重写了:\n%s", got)
	}
	// 5. 结果仍是合法 JSON,且语义正确。
	s, err := spec.Parse(out)
	if err != nil {
		t.Fatalf("回填后的 spec 解析失败:%v\n%s", err, got)
	}
	if len(s.Modules) != 2 || strings.Join(s.Modules[0].PartsOf(), ",") != "C11,C12,U3" {
		t.Fatalf("语义不对:%+v", s.Modules)
	}
}

func TestSpecPatchParts_RefusesWhenTargetMissing(t *testing.T) {
	// 半改一半的 spec 比没改的危险得多:定位不到就整体拒绝写。
	if _, err := specPatchParts([]byte(specFixture), map[string][]string{"NOPE": {"U9"}}); err == nil {
		t.Fatal("模块不存在却照写不误")
	}
	noParts := `{"modules":[{"name":"A","zone":"left"}]}`
	_, err := specPatchParts([]byte(noParts), map[string][]string{"A": {"U1"}})
	if err == nil || !strings.Contains(err.Error(), "parts") {
		t.Fatalf("缺 parts 键时应当拒绝并说清怎么办,got %v", err)
	}
}

func TestSpecPlanBackfill_MatchesByBlockThenName(t *testing.T) {
	s, err := spec.Parse([]byte(specFixture))
	if err != nil {
		t.Fatal(err)
	}
	groups := []specGroupSource{
		// 一个块拆成两个功能子群 —— 回填要把它们并起来。
		{Label: "esp32s3_wroom1_module(U3)", Name: "esp32s3_wroom1_module(U3)",
			BlockID: "esp32s3_wroom1_module", Members: []string{"U3", "C11"}},
		{Label: "esp32s3_wroom1_module(U3)/U_3V3", Name: "esp32s3_wroom1_module(U3)/U_3V3",
			BlockID: "esp32s3_wroom1_module", Members: []string{"C12"}},
		// 没有 block 的模块按组名末段 == zone/name 认。
		{Label: "g9", Name: "ldo(U7)/POWER", Members: []string{"U7", "C20"}},
	}
	want, res := specPlanBackfill(s, groups)
	if len(res.Changes) != 2 {
		t.Fatalf("changes=%d:%+v", len(res.Changes), res.Changes)
	}
	if strings.Join(want["MCU"], ",") != "C11,C12,U3" {
		t.Fatalf("MCU 没把两个子群并起来:%v", want["MCU"])
	}
	if strings.Join(want["POWER"], ",") != "C20,U7" {
		t.Fatalf("POWER 没按组名末段匹配:%v", want["POWER"])
	}
	// 差量必须报出来 —— 「块落位改写了哪几个位号」这句话的证据。
	for _, ch := range res.Changes {
		if ch.Module == "MCU" && (len(ch.Added) == 0 || len(ch.Removed) == 0) {
			t.Fatalf("MCU 的增删差量为空:%+v", ch)
		}
	}
}

func TestSpecPlanBackfill_UnchangedAndUnmatchedAreReported(t *testing.T) {
	s, _ := spec.Parse([]byte(specFixture))
	groups := []specGroupSource{
		// 位号本来就对(大小写/顺序不同不算变化 —— 位号是集合语义)。
		{Label: "g1", Name: "x", BlockID: "esp32s3_wroom1_module", Members: []string{"c2", "u1", "C1"}},
	}
	want, res := specPlanBackfill(s, groups)
	if len(want) != 0 {
		t.Fatalf("位号一致却要改写:%v", want)
	}
	if strings.Join(res.Unchanged, ",") != "MCU" {
		t.Fatalf("unchanged=%v", res.Unchanged)
	}
	// 匹配不上必须报出来:静默漏掉一个模块与写错位号的后果完全一样。
	if strings.Join(res.Unmatched, ",") != "POWER" {
		t.Fatalf("unmatched=%v", res.Unmatched)
	}
}

func TestSpecPlanBackfill_AmbiguousBlockIsSkippedNotGuessed(t *testing.T) {
	src := `{"modules":[
	  {"name":"LED_A","block":"led_indicator_gpio","parts":["LED1"]},
	  {"name":"LED_B","block":"led_indicator_gpio","parts":["LED2"]}
	]}`
	s, err := spec.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	groups := []specGroupSource{
		{Label: "g1", Name: "led_indicator_gpio(LED5)", BlockID: "led_indicator_gpio", Members: []string{"LED5", "R5"}},
	}
	want, res := specPlanBackfill(s, groups)
	if len(want) != 0 {
		t.Fatalf("歧义时替用户挑了一个:%v", want)
	}
	if len(res.Warnings) != 2 {
		t.Fatalf("歧义没有逐模块报出来:%v", res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "zone") {
		t.Fatalf("歧义警告没给出可执行的解法:%s", res.Warnings[0])
	}
}

// TestRunSpecBackfill_EndToEndOffline 走完整条离线路径:workflow 状态里的虚拟组
// → 回填计划 → 外科手术写入。这条路径不需要连接器,是它最大的优点之一。
func TestRunSpecBackfill_EndToEndOffline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(workflow.EnvDir, dir)

	st := &workflow.State{Project: "ceshi"}
	st.SetGroupsForPage("doc1", []*workflow.Group{{
		ID: "g1", Name: "esp32s3_wroom1_module(U3)",
		BlockID: "block.esp32s3_wroom1_module",
		Members: []string{"U3", "C11", "C12"},
		Roles:   map[string]string{"U": "U3"},
	}})
	if err := workflow.Save(st); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "s0.json")
	if err := os.WriteFile(path, []byte(specFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	res, patched, err := runSpecBackfill(path, "ceshi", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || res.Changes[0].Module != "MCU" {
		t.Fatalf("changes=%+v", res.Changes)
	}
	if err := specWriteAtomic(path, patched); err != nil {
		t.Fatal(err)
	}
	back, _ := os.ReadFile(path)
	var probe struct {
		XOwner  string `json:"x-owner"`
		Modules []struct {
			Name  string   `json:"name"`
			Parts []string `json:"parts"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(back, &probe); err != nil {
		t.Fatalf("写回的文件不是合法 JSON:%v\n%s", err, back)
	}
	if probe.XOwner != "mikas" {
		t.Fatalf("落盘丢了未知字段:\n%s", back)
	}
	if strings.Join(probe.Modules[0].Parts, ",") != "C11,C12,U3" {
		t.Fatalf("落盘的位号不对:%v", probe.Modules[0].Parts)
	}
	// 再跑一遍必须是无变化(幂等)—— 否则每次落块都会产生一次假 diff。
	res2, _, err := runSpecBackfill(path, "ceshi", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Changes) != 0 {
		t.Fatalf("回填不幂等:%+v", res2.Changes)
	}
}

func TestRunSpecBackfill_NoProjectIsAnActionableError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s0.json")
	if err := os.WriteFile(path, []byte(specFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runSpecBackfill(path, "", "")
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("缺 --project 时的报错要直接给出下一步,got %v", err)
	}
}

func TestRunSpecBackfill_NoGroupsWarnsInsteadOfSilentNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(workflow.EnvDir, dir)
	path := filepath.Join(t.TempDir(), "s0.json")
	if err := os.WriteFile(path, []byte(specFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _, err := runSpecBackfill(path, "nosuch", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "--project") {
		// "什么都没发生"必须与"一切正常"区分开,否则 --project 写错会静默无效。
		t.Fatalf("没有虚拟组时应当明确警告,got %+v", res.Warnings)
	}
}
