package app

// sch_check_partition_test.go — `missing-partition` 恒报的正负对照(自带假 daemon)。
//
// 复现的症状(issue #181 复盘):块驱动的页画好了分区框,`sch check` 每轮照报
// missing-partition,重跑多少次都不灭。上报者归因「虚拟组未持久化进 workflow」;
// 实测**不成立** —— 组是持久化了的(TestMissingPartition_VirtualGroupsArePersisted
// 用 block-apply 的同一条写路径钉住)。真根因是判据的**证据选错了**:它读的是
// 本地记账 SchZoneFrameIdsByPage,而记账按项目名分文件、会随 `--project` 写法 /
// 换机器 / 清缓存整份丢失,画布上的框对它隐形。
//
// 三组断言:
//
//	正对照 —— 记账丢了、画布上有区标题 → 不报(恒报消失)
//	负对照 —— 真没画框(有组没框 / 无组无框 / 只有说明没有标题)→ 照报
//	同一把尺 —— check 认标题用的字符串 = zone-draw 画标题写下的字符串

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// ── 同一把尺:认框的字符串必须就是画框写下的字符串 ─────────────────────────

// TestSchZoneTitle_MatchesDrawnTitle 把「check 认得出的标题」钉死在「zone-draw 真
// 写出去的标题」上。两条画框路径(批量 buildPartitionDrawJS / 韧性 partitionTargets)
// 各自 `strings.Join(p.Modules, " / ")`,本用例逐条比对 —— 哪天画框侧改了分隔符或
// 加了前缀,这里当场转红,而不是等到 check 又开始恒报。
func TestSchZoneTitle_MatchesDrawnTitle(t *testing.T) {
	plan := partitionPlan{Partitions: []partitionRect{
		{Modules: []string{"sy8089_buck_3v3"}, BBox: layoutBBox{100, 100, 300, 300}, TitleBBox: layoutBBox{100, 280, 300, 300}},
		{Modules: []string{"J_USB", "D_ESD"}, BBox: layoutBBox{320, 100, 520, 300}, TitleBBox: layoutBBox{320, 280, 520, 300}},
	}}
	targets := partitionTargets(plan, 22)
	if len(targets) != len(plan.Partitions) {
		t.Fatalf("partitionTargets 少给了目标:%d vs %d", len(targets), len(plan.Partitions))
	}
	js := buildPartitionDrawJS(plan, 22, "#FF0000")
	for i, p := range plan.Partitions {
		want := schZoneTitleContent(p.Modules)
		if targets[i].Title != want {
			t.Errorf("韧性路径写的标题 %q ≠ check 认的 %q", targets[i].Title, want)
		}
		if !strings.Contains(js, want) {
			t.Errorf("批量路径的 JS 里找不到标题 %q —— 两把尺", want)
		}
		// 认得回来才算闭环。
		set := schZoneNameSet([]string{"sy8089_buck_3v3", "J_USB", "D_ESD"})
		if !isSchZoneTitleText(want, set) {
			t.Errorf("check 认不出自己画的标题 %q", want)
		}
	}
}

// ── 纯核:证据合并与判定 ─────────────────────────────────────────────────

func TestSchPartitionEvidence_CanvasCoversLostBookkeeping(t *testing.T) {
	zones := []string{"POWER", "MCU", "USB"}
	texts := []zoneMoveText{
		{ID: "t1", Content: "POWER"},
		{ID: "t2", Content: "MCU"},
		{ID: "t3", Content: "USB"},
		{ID: "t4", Content: "5V 输入经 SY8089 降到 3V3,输出 2A"},
	}
	cases := []struct {
		name       string
		ev         schPartitionEvidence
		wantFrames int
		wantLabels int
	}{
		{
			name:       "记账丢了:画布作证",
			ev:         schPartitionEvidence{DrawnTitles: countDrawnZoneTitles(texts, zones), Zones: 3},
			wantFrames: 3, wantLabels: 3,
		},
		{
			name:       "记账在:照记账",
			ev:         schPartitionEvidence{RecordedRects: 3, RecordedLabels: 3, DrawnTitles: 3, Zones: 3},
			wantFrames: 3, wantLabels: 3,
		},
		{
			name:       "记账比画布多(框刚被人删了一个):取大,不因此改判",
			ev:         schPartitionEvidence{RecordedRects: 3, RecordedLabels: 3, DrawnTitles: 1, Zones: 3},
			wantFrames: 3, wantLabels: 3,
		},
		{
			name:       "两边都空",
			ev:         schPartitionEvidence{Zones: 3},
			wantFrames: 0, wantLabels: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.Frames(); got != tc.wantFrames {
				t.Errorf("Frames()=%d, want %d", got, tc.wantFrames)
			}
			if got := tc.ev.Labels(); got != tc.wantLabels {
				t.Errorf("Labels()=%d, want %d", got, tc.wantLabels)
			}
		})
	}
}

// TestCountDrawnZoneTitles_OnlyCountsRealTitles 是认标题这一步的鉴别力:说明、
// 未登记的区名、空串一律不算。认错了就等于把判据关掉。
func TestCountDrawnZoneTitles_OnlyCountsRealTitles(t *testing.T) {
	zones := []string{"POWER", "MCU", "J_USB", "D_ESD"}
	cases := []struct {
		content string
		want    bool
	}{
		{"POWER", true},
		{"power", true},             // 大小写折叠
		{"  MCU  ", true},           // 两端空白
		{"J_USB / D_ESD", true},     // 多模块合框
		{"D_ESD / J_USB", true},     // 次序无关
		{"POWER 电源模块说明", false},     // 说明里含模块名 ≠ 标题
		{"5V→3V3 降压,SY8089", false}, // 普通说明
		{"POWER / 未登记区", false},     // 有一段不是本页模块
		{"", false},
		{" / ", false},
		{"POWER / ", false},
	}
	set := schZoneNameSet(zones)
	for _, c := range cases {
		if got := isSchZoneTitleText(c.content, set); got != c.want {
			t.Errorf("isSchZoneTitleText(%q)=%v, want %v", c.content, got, c.want)
		}
	}
	texts := make([]zoneMoveText, 0, len(cases))
	for i, c := range cases {
		texts = append(texts, zoneMoveText{ID: fmt.Sprintf("t%d", i), Content: c.content})
	}
	if got, want := countDrawnZoneTitles(texts, zones), 5; got != want {
		t.Errorf("countDrawnZoneTitles=%d, want %d", got, want)
	}
	// 一页没有任何模块登记时,任何文本都不该被当成区标题(否则手搭页会被免检)。
	if got := countDrawnZoneTitles(texts, nil); got != 0 {
		t.Errorf("无模块登记时认出了 %d 条区标题,必须是 0", got)
	}
}

// TestPartitionFindingForZones_PositiveAndNegative 是判定这一层的正负对照。
func TestPartitionFindingForZones_PositiveAndNegative(t *testing.T) {
	const parts = 12
	cases := []struct {
		name        string
		frames      int
		labels      int
		textCount   int
		zones       int
		wantMissing bool
		wantNote    bool
	}{
		{"正对照:块驱动页,记账丢了但画布有 3 个框 + 3 条说明", 3, 3, 6, 3, false, false},
		{"负对照 A:有虚拟组但一个框都没画", 0, 0, 0, 3, true, true},
		{"负对照 B:一片散件,无组无框", 0, 0, 0, 0, true, true},
		{"负对照 C:只写了说明没画框(标题数为 0)", 0, 0, 3, 3, true, false},
		{"负对照 D:画了框但一条说明没有", 3, 3, 3, 3, false, true},
		{"低于阈值:一条都不报", 0, 0, 0, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := parts
			if strings.HasPrefix(tc.name, "低于阈值") {
				n = schPartitionMinParts - 1
			}
			got := partitionFindingForZones(n, tc.frames, tc.labels, tc.textCount, tc.zones)
			var missing, note *checkFinding
			for _, f := range got {
				switch f.Type {
				case "missing-partition":
					missing = f
				case "missing-note":
					note = f
				}
			}
			if (missing != nil) != tc.wantMissing {
				t.Errorf("missing-partition present=%v, want %v (findings=%d)", missing != nil, tc.wantMissing, len(got))
			}
			if (note != nil) != tc.wantNote {
				t.Errorf("missing-note present=%v, want %v", note != nil, tc.wantNote)
			}
			// 报文必须给出**这一页真正能执行的下一步**:已归组的页别再劝人去 `zones set`。
			if missing != nil && tc.zones > 0 {
				if strings.Contains(missing.Message, "sch zones set") {
					t.Errorf("已归组的页仍在劝 `sch zones set`:%s", missing.Message)
				}
				if !strings.Contains(missing.Message, "zone-arrange") {
					t.Errorf("已归组页的报文没给出 partitionOverlap 的出路:%s", missing.Message)
				}
			}
		})
	}
}

// ── 提示行按真实类型给,不按聚合槽给 ──────────────────────────────────────
//
// 交付三件套(partition/note/titleblock)共用 Summary.MissingPartitions 一个槽。
// 提示行原来读那个槽,于是**只有图签没填**的页也会印出「没画功能分区框」——
// 图签写入长期挂账,这一行就永远亮着,而且指错方向。这正是「missing-partition
// 恒报」被人看见的那一面。
func TestMissingDeliverableHints_PerRealType(t *testing.T) {
	cases := []struct {
		name     string
		findings []checkFinding
		want     []string // 必须出现的关键词
		absent   []string
	}{
		{
			name:     "只有图签没填:不许说没画分区框",
			findings: []checkFinding{{Type: "missing-titleblock"}},
			want:     []string{"missing-titleblock"},
			absent:   []string{"missing-partition", "missing-note"},
		},
		{
			name:     "只有没画框",
			findings: []checkFinding{{Type: "missing-partition"}},
			want:     []string{"missing-partition", "zone-draw"},
			absent:   []string{"missing-titleblock", "missing-note"},
		},
		{
			name:     "只有没说明",
			findings: []checkFinding{{Type: "missing-note"}},
			want:     []string{"missing-note", "sch note"},
			absent:   []string{"missing-partition", "missing-titleblock"},
		},
		{
			name: "三条齐:三条提示都给,顺序固定",
			findings: []checkFinding{
				{Type: "missing-titleblock"}, {Type: "missing-note"}, {Type: "missing-partition"},
			},
			want: []string{"missing-partition", "missing-note", "missing-titleblock"},
		},
		{
			name:     "与交付三件套无关的 finding 不触发任何提示",
			findings: []checkFinding{{Type: "floating-pin"}, {Type: "marker-overlap"}},
			absent:   []string{"missing-partition", "missing-note", "missing-titleblock"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			joined := strings.Join(missingDeliverableHints(tc.findings), "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("提示里缺 %q:\n%s", w, joined)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(joined, a) {
					t.Errorf("提示里多出 %q(指错方向):\n%s", a, joined)
				}
			}
		})
	}
	// 顺序:框 → 说明 → 图签。
	all := missingDeliverableHints([]checkFinding{
		{Type: "missing-titleblock"}, {Type: "missing-note"}, {Type: "missing-partition"},
	})
	if len(all) != 3 ||
		!strings.Contains(all[0], "missing-partition") ||
		!strings.Contains(all[1], "missing-note") ||
		!strings.Contains(all[2], "missing-titleblock") {
		t.Fatalf("提示顺序不对:%v", all)
	}
}

// TestRenderCheckReport_TitleblockOnlyDoesNotSayPartition 从渲染出口再验一遍:
// 一份**只有 missing-titleblock** 的报告,整段输出里不许出现「没画功能分区框」。
func TestRenderCheckReport_TitleblockOnlyDoesNotSayPartition(t *testing.T) {
	rep := checkReport{
		Findings: []checkFinding{{
			Type: "missing-titleblock", Level: "warn", Count: 3,
			Message: "图签未填:Name(图纸标题)、Drawed(设计者)、Description(图纸说明)",
		}},
	}
	rep.Summary.MissingPartitions = 1
	rep.Summary.Total = 1
	var out strings.Builder
	renderCheckReport(rep, &out)
	got := out.String()
	if strings.Contains(got, "没画功能分区框") {
		t.Fatalf("只有图签没填,报告却说没画分区框:\n%s", got)
	}
	if !strings.Contains(got, "missing-titleblock") {
		t.Fatalf("图签的提示行没给:\n%s", got)
	}
	// 汇总行仍然照口径写成 missing-deliverable(2026-08-17 的订正不许被本次改掉)。
	if !strings.Contains(got, "missing-deliverable") {
		t.Fatalf("汇总行的 missing-deliverable 口径丢了:\n%s", got)
	}
}

// ── 端到端(假 daemon + 临时 workflow 目录):证据真的接到了判据上 ────────────

// partitionCheckFake 是一台只回答本判据需要的三件事的假 daemon:
// /health(找窗口)、document.current(钉页)、schematic.text.list(画布文本)。
type partitionCheckFake struct {
	texts []map[string]any
}

func newPartitionCheckFake(t *testing.T, texts []map[string]any) *appConfig {
	t.Helper()
	f := &partitionCheckFake{texts: texts}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"service":"easyeda-agent","status":"ok","windows":[{"windowId":"w1","context":{"projectName":"parttest"}}]}`)
	})
	mux.HandleFunc("/action", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Action {
		case "document.current":
			fmt.Fprint(w, `{"ok":true,"result":{},"context":{"projectName":"parttest","documentType":"schematic","documentUuid":"page-1"}}`)
		case "schematic.text.list":
			b, _ := json.Marshal(map[string]any{"count": len(f.texts), "scope": "activePage", "texts": f.texts})
			fmt.Fprintf(w, `{"ok":true,"result":%s}`, b)
		default:
			fmt.Fprint(w, `{"ok":true,"result":{}}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u := strings.TrimPrefix(srv.URL, "http://")
	i := strings.LastIndex(u, ":")
	if i < 0 {
		t.Fatalf("bad test server url %q", srv.URL)
	}
	host, port := u[:i], u[i+1:]
	return &appConfig{host: host, ports: port + "-" + port, project: "parttest"}
}

// writeBlockDrivenState 造一份**块驱动页**的 workflow:三个虚拟组(block-apply 的
// 形态:块实例 + 子组),而分区框记账**一个都没有**(记账丢失的那三条路径的终态)。
func writeBlockDrivenState(t *testing.T, withFrameRecord bool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(workflow.EnvDir, dir)
	st := &workflow.State{
		Project: "parttest",
		GroupsByPage: map[string][]*workflow.Group{
			"page-1": {
				{ID: "g1", Name: "sy8089_buck_3v3(U1)", Members: []string{"U1", "C1", "C2", "L1"}, BlockID: "sy8089_buck_3v3"},
				{ID: "g2", Name: "ch340c_usb_serial(U3)/J_USB", Members: []string{"J2", "R5"}, BlockID: "ch340c_usb_serial"},
				{ID: "g3", Name: "ch340c_usb_serial(U3)/D_ESD", Members: []string{"D1"}, BlockID: "ch340c_usb_serial"},
			},
		},
	}
	if withFrameRecord {
		st.SchZoneFrameIdsByPage = map[string]*workflow.SchZoneFrames{
			"page-1": {DocumentUUID: "page-1", Mode: "partition",
				Rects: []string{"r1", "r2", "r3"}, Texts: []string{"x1", "x2", "x3"}},
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "parttest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// twelveParts 造一页 12 个器件 —— 越过 schPartitionMinParts,判据才会开口。
func twelveParts() []layoutComp {
	out := make([]layoutComp, 0, 12)
	for i := 0; i < 12; i++ {
		out = append(out, layoutComp{ComponentType: schLayoutPartType, Designator: fmt.Sprintf("R%d", i+1)})
	}
	return out
}

func textPrim(id, content string) map[string]any {
	return map[string]any{"primitiveId": id, "content": content, "x": 10.0, "y": 20.0}
}

// TestPartitionFinding_LiveEvidence 端到端跑 partitionFinding:同一份「块驱动 +
// 记账丢失」的页,画布上有没有区标题决定报不报。这一层证明证据真的**接上了**
// 判据(纯核对了、外壳没接上,恒报照旧)。
func TestPartitionFinding_LiveEvidence(t *testing.T) {
	t.Run("正对照:画布上有三个区标题 → 不报 missing-partition", func(t *testing.T) {
		writeBlockDrivenState(t, false)
		cfg := newPartitionCheckFake(t, []map[string]any{
			textPrim("t1", "sy8089_buck_3v3(U1)"),
			textPrim("t2", "J_USB"),
			textPrim("t3", "D_ESD"),
			textPrim("t4", "5V 输入经 SY8089 降到 3V3"),
			textPrim("t5", "USB 口 D+/D- 串 ESD 后进 CH340C"),
			textPrim("t6", "ESD 阵列贴 USB 口放,钳位到 GND"),
		})
		var errBuf strings.Builder
		got := partitionFinding(cfg, "", twelveParts(), &errBuf)
		for _, f := range got {
			if f.Type == "missing-partition" {
				t.Fatalf("恒报没消:%s(stderr=%s)", f.Message, errBuf.String())
			}
		}
		for _, f := range got {
			if f.Type == "missing-note" {
				t.Fatalf("三条说明在画布上,却报 missing-note:%s", f.Message)
			}
		}
	})

	t.Run("负对照 A:同一页,画布上只有说明没有区标题 → 照报", func(t *testing.T) {
		writeBlockDrivenState(t, false)
		cfg := newPartitionCheckFake(t, []map[string]any{
			textPrim("t4", "5V 输入经 SY8089 降到 3V3"),
			textPrim("t5", "USB 口 D+/D- 串 ESD 后进 CH340C"),
		})
		var errBuf strings.Builder
		got := partitionFinding(cfg, "", twelveParts(), &errBuf)
		var missing *checkFinding
		for _, f := range got {
			if f.Type == "missing-partition" {
				missing = f
			}
		}
		if missing == nil {
			t.Fatalf("真没画框的页不报了 —— 判据被改瞎(findings=%d, stderr=%s)", len(got), errBuf.String())
		}
		if !strings.Contains(missing.Message, "3 个功能模块") {
			t.Errorf("报文没说清这页已归组:%s", missing.Message)
		}
	})

	t.Run("负对照 B:画布全空 → 照报,且 missing-note 一并报", func(t *testing.T) {
		writeBlockDrivenState(t, false)
		cfg := newPartitionCheckFake(t, nil)
		var errBuf strings.Builder
		got := partitionFinding(cfg, "", twelveParts(), &errBuf)
		types := map[string]bool{}
		for _, f := range got {
			types[f.Type] = true
		}
		if !types["missing-partition"] || !types["missing-note"] {
			t.Fatalf("空白页应当两条都报,实际 %v(stderr=%s)", types, errBuf.String())
		}
	})

	t.Run("记账还在时不受影响:老路径照旧放行", func(t *testing.T) {
		writeBlockDrivenState(t, true)
		cfg := newPartitionCheckFake(t, []map[string]any{
			textPrim("t4", "5V 输入经 SY8089 降到 3V3"),
		})
		var errBuf strings.Builder
		got := partitionFinding(cfg, "", twelveParts(), &errBuf)
		for _, f := range got {
			if f.Type == "missing-partition" {
				t.Fatalf("有记账仍报 missing-partition:%s", f.Message)
			}
		}
	})
}

// TestMissingPartition_VirtualGroupsArePersisted 订正上报者的归因:虚拟组**是**
// 持久化的。block-apply 走的就是 saveSchGroups → State.GroupsByPage 这条路,
// 存下去、读回来、投影成模块表全都成立 —— 所以恒报不可能是「组没存下来」。
func TestMissingPartition_VirtualGroupsArePersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(workflow.EnvDir, dir)
	st, err := loadPcbStageState("parttest")
	if err != nil {
		t.Fatal(err)
	}
	groups := []*schGroup{
		{ID: "g1", Name: "sy8089_buck_3v3(U1)", Members: []string{"U1", "C1"}},
		{ID: "g2", Name: "ch340c_usb_serial(U3)/J_USB", Members: []string{"J2"}},
	}
	if err := saveSchGroups(st, "page-1", groups); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadPcbStageState("parttest")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.GroupsForPage("page-1")); got != 2 {
		t.Fatalf("虚拟组没落盘:重读到 %d 个组,want 2 —— 若此处转红,上报者的归因才成立", got)
	}
	zones := schGroupModulesFromState(reloaded, "page-1")
	if len(zones) != 2 {
		t.Fatalf("组投影成模块表失败:%d", len(zones))
	}
	// 区名取组名末段 —— 与画布上的标题、`sch note --zone` 的写回口径同一套。
	if _, ok := zones["J_USB"]; !ok {
		t.Fatalf("子组区名不是末段:%v", zones)
	}
	// 而分区框记账是**另一张表**,它是空的 —— 这正是恒报的位置。
	if f, _ := recordedZoneFrames(reloaded, "page-1"); f != nil {
		t.Fatalf("fixture 失效:不该有框记账,却有 %+v", f)
	}
}
