package app

// sch_check_partition.go — `missing-partition` 的**证据来源**:画布优先,记账兜底。
//
// ── 恒报的根因(issue #181 复盘:「missing-partition 恒报 2,每轮 check 都亮」)──
//
// 上报者的归因是「虚拟组未持久化进 workflow」。**这条归因不成立** —— `block-apply`
// 落块时确实把虚拟组写进了 workflow(cmd_sch_block_apply_run.go 的 saveSchGroups →
// State.GroupsByPage),本机 `ceshi.json` 三页各有 4/2/6 个组为证。
//
// 真根因在**证据本身选错了**:`missing-partition` 判的从来不是「这页画没画框」,
// 而是「**我们的本地记账里有没有这页的框 id**」(State.SchZoneFrameIdsByPage)。
// 那份记账有三条与画布无关的丢失路径,任何一条都让一页画好的框对 check 隐形:
//
//	① 记账按**项目名**分文件(workflow/<project>.json),而 resolveStageProject
//	   **优先用 `--project` 里用户敲的那个字符串**,拿不到才回落 friendlyName /
//	   uuid。同一个工程用 `--project ceshi` 画框、之后不带 `--project`(解析成
//	   friendlyName 或 uuid)跑 check,读的就是另一个文件 —— 本机 workflow 目录里
//	   同时存在按名字的 `BBClaw-AI.json` 和按 uuid 的
//	   `307b0022264f44c1beb4ba9355421ce9.json`,就是这条路径的化石。
//	② 换机器 / 清 `~/.easyeda-agent` / 换用户:画布上的框还在,记账没了。
//	③ 框由记账之外的路径落到画布(历史版本、手工、compensate 后的残留)。
//
// 三条都产生同一个形态:**画布上有框,check 说没有,而且怎么重跑都还是没有**——
// 亮黄灯的恒报。恒亮又永不该管的判据比没有判据更糟:它训练人忽略整类告警。
//
// ── 修法:判定与生成用同一把尺 ────────────────────────────────────────────
//
// zone-draw 画一个区 = 一个矩形 + 一条标题文本,标题内容恒为
// `strings.Join(p.Modules, " / ")`(cmd_sch_zone_plan.go:887 与
// cmd_sch_zone_draw_resilient.go:56 两条画框路径逐字相同),而 `sch check` 本来
// 就已经把整页 `schematic.text.list` 拉下来了(missing-note 要用)。于是**认框
// 不需要任何新 I/O**:画布上出现一条内容正好是本页某个(或某几个)模块名的文本,
// 就是「这一区画过框」的直接证据 —— 用的正是画框那一侧生成标题的同一个函数
// (schZoneTitleContent),同一把尺,不是第二套启发式。
//
// 记账仍然读(recordedZoneFrames,与 zone-draw 自己的查找口径同一个函数),两个
// 证人取**大**:记账在就按记账,记账丢了画布还能作证。
//
// ── 不许被这次容错顺手改瞎的两条 ──────────────────────────────────────────
//
//	(a) 真没画框的页**必须照报**:画布上没有区标题、记账也空 → 一个字不放宽。
//	    「有虚拟组」本身**不算**分区完成 —— 铁律#15 要的是①分页②画框③说明,
//	    SKILL 明写「手工 block-apply/sch place 不自动画框,必须补②③」。若让
//	    「有组即免检」,block-apply 是主力落件路径,这条判据当场等于删掉。
//	(b) 认出来的区标题要**同时**记进 labelTexts:missing-note 的口径是
//	    「自由文本数 − 区名标签数」,只补框不补标签,标题就会被当成电路说明,
//	    把 missing-note 静默关掉(判据不能一边松一边更松)。

import "strings"

// missingDeliverableHints 按**真实出现的类型**给交付三件套的提示行(纯函数)。
// 见 renderCheckReport 里的说明:三条共用一个聚合计数槽,提示行若按槽给,就会
// 把 missing-titleblock 说成「没画分区框」。顺序固定:框 → 说明 → 图签。
func missingDeliverableHints(findings []checkFinding) []string {
	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.Type] = true
	}
	var out []string
	if seen["missing-partition"] {
		out = append(out, "→ missing-partition: 多器件页没画功能分区框(铁律#15) — `sch zones set`→`sch zone-draw`(整纸版式 --mode partition);"+
			"若 `sch zone-plan` 报 partitionOverlap,先 `sch zone-arrange --apply` 或拆页,再画")
	}
	if seen["missing-note"] {
		out = append(out, "→ missing-note: 多器件页没有电路说明(区名标签不算,铁律#15) — 每模块 `sch note --zone <区>` 加 1~3 行:作用 + 关键参数 + 设计要点")
	}
	if seen["missing-titleblock"] {
		// 文案与 titleBlockFinding 的明细逐字同源,免得提示行和明细行教两套写法。
		out = append(out, "→ missing-titleblock: 图签必填项空着,交付图必须能认领 — `sch titleblock --data '{\"Name\":\"…\",\"Drawed\":\"…\",\"Description\":\"…\"}'`")
	}
	return out
}

// schZoneTitleSep 是区标题里多个模块名之间的分隔符。zone-draw 的两条画框路径
// 都用 `strings.Join(p.Modules, " / ")`;这里把那个字面量收成一个名字,check
// 侧靠它反认。(两条画框路径在 cmd_sch_zone_* 里,本次不许改动,所以由
// TestSchZoneTitle_MatchesDrawnTitle 钉住「两边一个字不差」。)
const schZoneTitleSep = " / "

// schZoneTitleContent 生成一个分区框的标题文本 —— 与 zone-draw 落到画布上的内容
// 逐字相同。
func schZoneTitleContent(modules []string) string {
	return strings.Join(modules, schZoneTitleSep)
}

// schZoneNameKey 归一化一个区名用于比较:去空白 + 折大小写。区名来自虚拟组名末段
// 或认领表的 key,写进画布时原样;折大小写只是吸收人手敲认领时的大小写差异。
func schZoneNameKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// isSchZoneTitleText 判一条画布文本**是不是**本页某个分区框的标题:按 " / " 拆开
// 之后,每一段都必须是本页登记过的模块名(空段直接否决)。
//
// 为什么要求「每一段都是」而不是「含有」:电路说明里出现模块名是常事,而一条
// 内容恰好等于「模块名」或「模块名 / 模块名」的自由文本,只有 zone-draw 会写。
func isSchZoneTitleText(content string, zoneNames map[string]bool) bool {
	if len(zoneNames) == 0 {
		return false
	}
	segs := strings.Split(content, schZoneTitleSep)
	if len(segs) == 0 {
		return false
	}
	for _, s := range segs {
		key := schZoneNameKey(s)
		if key == "" || !zoneNames[key] {
			return false
		}
	}
	return true
}

// schZoneNameSet 把本页模块名列表折成查找集。
func schZoneNameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		if k := schZoneNameKey(n); k != "" {
			out[k] = true
		}
	}
	return out
}

// countDrawnZoneTitles 数画布上有几条区标题 —— 即「这页实际画出了几个分区框」。
// 同一条内容出现多次照数多次:zone-draw 每个区写一条,重复内容意味着重复的框。
func countDrawnZoneTitles(texts []zoneMoveText, zoneNames []string) int {
	set := schZoneNameSet(zoneNames)
	if len(set) == 0 {
		return 0
	}
	n := 0
	for _, t := range texts {
		if isSchZoneTitleText(t.Content, set) {
			n++
		}
	}
	return n
}

// schPartitionEvidence 是一页「分区做到哪一步」的全部证据,两个证人分开留痕,
// 便于报文说清楚是谁作的证。
type schPartitionEvidence struct {
	// RecordedRects / RecordedLabels 来自本地记账(zone-draw 写、recordedZoneFrames 读)。
	RecordedRects  int
	RecordedLabels int
	// DrawnTitles 来自画布(text.list 里认出来的区标题数)。
	DrawnTitles int
	// Zones 是本页登记的功能模块数(虚拟组优先,没有组才是认领表)。
	// 它只决定**报文怎么写**,不决定报不报 —— 有组不等于画了框。
	Zones int
}

// Frames 是「这页有几个分区框」的最终口径:两个证人取大。
func (e schPartitionEvidence) Frames() int {
	if e.DrawnTitles > e.RecordedRects {
		return e.DrawnTitles
	}
	return e.RecordedRects
}

// Labels 是「这页有几条区名标签」的最终口径(missing-note 拿它做减数)。
func (e schPartitionEvidence) Labels() int {
	if e.DrawnTitles > e.RecordedLabels {
		return e.DrawnTitles
	}
	return e.RecordedLabels
}

// schPartitionPageEvidence 是 I/O 外壳:一次 loadSchGroupsContext 取回记账 + 本页
// 模块表,再拿调用方已经拉好的 text.list 认画布上的区标题(零新增 I/O)。
// best-effort:读不到上下文就退回全 0 —— 等价于修复前的行为,绝不因此漏报。
func schPartitionPageEvidence(cfg *appConfig, window string, texts []zoneMoveText) schPartitionEvidence {
	_, _, docUUID, _, st, _, err := loadSchGroupsContext(cfg, window)
	if err != nil || st == nil {
		return schPartitionEvidence{}
	}
	// 模块归属的读法与 loadSchZoneModules 一致:虚拟组优先,没有组才回落认领。
	zones := schGroupModulesFromState(st, docUUID)
	if len(zones) == 0 {
		zones = st.SchZonesForPage(docUUID)
	}
	names := make([]string, 0, len(zones))
	for n := range zones {
		names = append(names, n)
	}
	ev := schPartitionEvidence{Zones: len(zones), DrawnTitles: countDrawnZoneTitles(texts, names)}
	// recordedZoneFrames 是 zone-draw 自己查记账的那个函数 —— 记账这一侧也共用一把尺
	// (旧的 schZoneFrameCounts 自己手写了一遍 page/legacy 两分支,legacy 的空记录
	// 判定与 zone-draw 不同)。
	if f, _ := recordedZoneFrames(st, docUUID); f != nil {
		ev.RecordedRects, ev.RecordedLabels = len(f.Rects), len(f.Texts)
	}
	return ev
}
