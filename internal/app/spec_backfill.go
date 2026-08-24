package app

// spec_backfill.go — 把**落地后的真实位号**回填进 S0 spec 的 modules[].parts。
//
// ## 立项现场(#181 第三份复盘,排第 3 的卡点)
//
//	spec 位号漂移回填(每次落块回头改 json,×2+ 轮)—— 块实例落位改写模块位号,
//	分区判据读不到,必须手动改 S0 json 的 parts。
//
// 漂移的机制不是 bug,是**平台的正当行为 + 我们的正确应对**叠出来的:
// `sch block-apply` 计划 C1,EasyEDA 在 create 时按它自己知道的全局位号重编成 C11
// (它看得见我们的预扫描看不见的未加载页,issue #144),我们照实回读并 remap
// (bapRemapDesignators)。于是画布上是 C11、虚拟组里记的是 C11 —— 只有**手写的
// S0 spec 里还是 C1**。而 `modules[].parts` 是**designator 字符串**,分区归属
// (pcb zones set)、partition 打分(partScoreFromSpec)、连接器规则全部按它做键:
// 键对不上不会报错,只会**静默少算一个模块**,报告照样绿。
//
// ## 事实来源:虚拟组,不是画布
//
// 回填读的是 workflow 状态里的**持久虚拟组**(workflow.Group):block-apply 在
// 归组时就把 `BlockID` / `Instance` / `Roles(ROLE→落地位号)` / `Members` 写进去了,
// 而且写的正是 remap 之后的真实位号(见 bapRegisterGroup 的注释)。所以:
//
//   - 回填**完全离线**——不需要连接器、不需要打开 EasyEDA;
//   - 不会与画布产生第三份真相(组表本来就是模块归属的单一事实来源,
//     见 loadSchZoneModules 的注释)。
//
// ## 写法:外科手术,绝不整包重写
//
// memory「attrs-backfill 投影键灭位号」那次事故的教训不是"某个键有毒",是
// **整体写会抹掉你没读进来的东西**:平台把 otherProperty 当整对象替换,我们
// 把库记录整包灌回去,顺手把 166 个真实位号灌成了 `C?`。
//
// 同一条教训在这里的形式是:**绝不 Unmarshal 到 spec.Spec 再 Marshal 回去**。
// spec.Spec 只覆盖它认识的字段,整包写回会静默丢掉:注释性的自定义键、未来版本
// 新增的字段、`notes` 里的结构、以及全部键序与缩进(一个手写文件被重排成字母序,
// diff 里就是"整个文件都变了",而真正改的只有一行)。
//
// 所以 specPatchParts 在**原始字节**上只替换 `modules[i].parts` 的值:用
// json.Decoder 的 InputOffset 精确定位那一段,其余一个字节都不碰。写不动就如实
// 报告并**不写**,绝不"尽力而为"地重排整个文件。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// specBackfillManualHint 拼「手工同步」那一行提示。
//
// 这行提示存在的唯一理由就是**给人照抄**,所以 project 为空时绝不能拼出
// `--project  --write`(两个空格,占位符没填上)—— 抄下来就是一条必然跑不通的
// 命令,比不给提示更糟:人会以为自己抄错了,而不是工具没算出来。
//
// 解析不出工程名时改成说清楚**怎样才能得到它**,并给出不需要工程名的那条路
// (`--window <id>`,与 block-apply 的路由方式一致)。
func specBackfillManualHint(path, project string) string {
	if p := strings.TrimSpace(project); p != "" {
		return fmt.Sprintf("手工同步:easyeda spec backfill %s --project %s --write", path, p)
	}
	return fmt.Sprintf("手工同步:工程名 = `easyeda project info --window <id>` 里的 friendlyName"+
		"(或 `easyeda health` 里那个窗口的 projectName),拿到后跑 "+
		"`easyeda spec backfill %s --project <工程名> --write`;"+
		"也可以直接把窗口给它:`easyeda spec backfill %s --window <id> --write`", path, path)
}

// specBackfillChange 是一个模块的回填结果。
type specBackfillChange struct {
	Module string   `json:"module"`
	Block  string   `json:"block,omitempty"`
	Before []string `json:"before"`
	After  []string `json:"after"`
	// Added / Removed 是给人看的差量 —— 「块落位改写了哪几个位号」这句话的证据。
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Groups 是提供 After 的虚拟组(id 或名字),可追溯到底是哪次 block-apply。
	Groups []string `json:"groups,omitempty"`
}

// specBackfillResult 是命令输出。
type specBackfillResult struct {
	Spec    string               `json:"spec"`
	Project string               `json:"project,omitempty"`
	Changes []specBackfillChange `json:"changes"`
	// Unchanged 是匹配上了但位号本来就对的模块 —— 必须列出来,否则读的人分不清
	// 「对上了没变」和「根本没匹配上」,而后者才是要修的。
	Unchanged []string `json:"unchanged,omitempty"`
	// Unmatched 是 spec 里有、却在组表里找不到对应块/组的模块。它们**不是错误**
	// (手工连线的模块本来就没有虚拟组),但必须报出来:回填静默漏掉一个模块,
	// 后果与位号写错完全一样。
	Unmatched []string `json:"unmatched,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Written   bool     `json:"written"`
}

// specGroupSource 是一个候选事实来源(一个虚拟组)。
type specGroupSource struct {
	Label    string // 组 id(g1)或组名,报告用
	Name     string // 组名(可能带 `实例/子群` 分段)
	BlockID  string // 归一化后的块短名(去 block. 前缀)
	Instance string
	Members  []string
}

// specCollectGroups 把 workflow 状态里所有页的虚拟组折成候选来源。
//
// **跨页收集是有意的**:S0 的模块与原理图分页是两个维度(一个模块可能被拆到
// 两页,或者干脆被移到了别的页)。按页过滤会让「模块搬了页 → 回填悄悄漏掉它」,
// 而那正是回填要根治的那类静默失效。
//
// **但跨页也正是同名重建污染的唯一入口**:状态文件按工程名分文件,`ceshi` 删掉
// 重建三次就把四个工程的页堆进一个文件;页级读取(GroupsForPage)天然免疫(死
// 工程的 documentUuid 配不上活页),而这里会把死页的组正常收进来 —— 同一个块在
// 死页和活页各有一个组,回填就把两页的 members 并起来,写出一份画布上根本不存在
// 的位号表,全程零报错。所以按 liveUUID 收窄:**已证明属于别的工程**的页不参与。
// 收窄掉的页必须返回给调用方报出来 —— 静默地少收几页,与静默地多收几页一样坏。
//
// liveUUID 为空(离线且文件里也没记过 uuid)时不收窄,行为与收窄前完全一致。
func specCollectGroups(st *workflow.State, liveUUID string) (sources []specGroupSource, skippedPages []string) {
	if st == nil {
		return nil, nil
	}
	var out []specGroupSource
	pages := make([]string, 0, len(st.GroupsByPage))
	for p := range st.GroupsByPage {
		pages = append(pages, p)
	}
	sort.Strings(pages) // 同输入同输出
	for _, p := range pages {
		if !st.PageInScope(p, liveUUID) {
			if len(st.GroupsByPage[p]) > 0 {
				skippedPages = append(skippedPages, p)
			}
			continue
		}
		for _, g := range st.GroupsByPage[p] {
			if g == nil || len(g.Members) == 0 {
				continue
			}
			label := strings.TrimSpace(g.Name)
			if label == "" {
				label = g.ID
			}
			out = append(out, specGroupSource{
				Label:    label,
				Name:     g.Name,
				BlockID:  strings.TrimPrefix(strings.TrimSpace(g.BlockID), "block."),
				Instance: strings.TrimSpace(g.Instance),
				Members:  append([]string(nil), g.Members...),
			})
		}
	}
	return out, skippedPages
}

// specMatchGroups 给一个 spec 模块挑出它的事实来源。
//
// 两条匹配规则,**声明优先于猜测**:
//  1. `module.block` —— 模块自己声明了它是哪个块,这是最强的证据;
//  2. 组名末段 == `module.zone` 或 `module.name` —— block-apply 的子群名就是区名
//     (schGroupModulesFromState 的口径),手工建的组也常直接用模块名。
//
// 只要规则 1 命中过,就**不再**看规则 2:两条规则同时生效会把「同名巧合」混进
// 声明的结果里,而回填是写文件的操作,宁可少匹配也不许错匹配。
func specMatchGroups(m spec.Module, groups []specGroupSource) []specGroupSource {
	block := strings.TrimPrefix(strings.TrimSpace(m.Block), "block.")
	if block != "" {
		var hit []specGroupSource
		for _, g := range groups {
			if g.BlockID != "" && strings.EqualFold(g.BlockID, block) {
				hit = append(hit, g)
			}
		}
		return hit
	}
	wanted := []string{strings.TrimSpace(m.Zone), strings.TrimSpace(m.Name)}
	var hit []specGroupSource
	for _, g := range groups {
		last := g.Name
		if i := strings.LastIndex(last, "/"); i >= 0 && i+1 < len(last) {
			last = last[i+1:]
		}
		for _, w := range wanted {
			if w != "" && strings.EqualFold(strings.TrimSpace(last), w) {
				hit = append(hit, g)
				break
			}
		}
	}
	return hit
}

// specPlanBackfill 是纯核:给定 spec 与组表,算出每个模块的新 parts。无 I/O,可单测。
//
// 返回 (module 名 → 新 parts, 结果报告)。**只把有变化的模块放进 want** ——
// 没变化的模块一个字节都不该被改写(见文件头「外科手术」)。
func specPlanBackfill(s *spec.Spec, groups []specGroupSource) (map[string][]string, specBackfillResult) {
	var res specBackfillResult
	want := map[string][]string{}
	if s == nil {
		return want, res
	}
	// 同一个块被两个模块声明 = 歧义。此时**两个都跳过**并报出来:自动挑一个
	// 等于替用户做了他没做的设计决定,而这份文件是设计意图的正本。
	blockClaims := map[string][]string{}
	for _, m := range s.Modules {
		if b := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(m.Block), "block.")); b != "" {
			blockClaims[b] = append(blockClaims[b], m.Name)
		}
	}
	for _, m := range s.Modules {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		blockKey := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(m.Block), "block."))
		if blockKey != "" && len(blockClaims[blockKey]) > 1 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"模块 %s 跳过:块 %q 被 %d 个模块同时声明(%s)—— 分不清哪个组归谁,"+
					"请给它们各写一个 zone(或改用不同的块实例)后重跑",
				name, m.Block, len(blockClaims[blockKey]), strings.Join(blockClaims[blockKey], ", ")))
			continue
		}
		hits := specMatchGroups(m, groups)
		if len(hits) == 0 {
			res.Unmatched = append(res.Unmatched, name)
			continue
		}
		seen := map[string]bool{}
		var parts, labels []string
		for _, g := range hits {
			labels = append(labels, g.Label)
			for _, d := range g.Members {
				u := strings.ToUpper(strings.TrimSpace(d))
				if u == "" || seen[u] {
					continue
				}
				seen[u] = true
				parts = append(parts, u)
			}
		}
		sort.Strings(parts) // 确定性:同一份状态回填出同一份文件
		sort.Strings(labels)
		before := m.PartsOf()
		if specSameParts(before, parts) {
			res.Unchanged = append(res.Unchanged, name)
			continue
		}
		add, del := specPartsDiff(before, parts)
		res.Changes = append(res.Changes, specBackfillChange{
			Module: name, Block: strings.TrimSpace(m.Block),
			Before: before, After: parts, Added: add, Removed: del, Groups: labels,
		})
		want[name] = parts
	}
	sort.Strings(res.Unchanged)
	sort.Strings(res.Unmatched)
	return want, res
}

// specSameParts 判两份位号表是否等价(大小写与顺序无关 —— 位号是集合语义)。
func specSameParts(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ua, ub := specUpperSorted(a), specUpperSorted(b)
	for i := range ua {
		if ua[i] != ub[i] {
			return false
		}
	}
	return true
}

func specUpperSorted(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// specPartsDiff 给出 before→after 的增删(报告用;判据仍是 specSameParts)。
func specPartsDiff(before, after []string) (added, removed []string) {
	has := func(list []string, v string) bool {
		for _, s := range list {
			if strings.EqualFold(strings.TrimSpace(s), v) {
				return true
			}
		}
		return false
	}
	for _, a := range specUpperSorted(after) {
		if !has(before, a) {
			added = append(added, a)
		}
	}
	for _, b := range specUpperSorted(before) {
		if !has(after, b) {
			removed = append(removed, b)
		}
	}
	return added, removed
}

// ── 外科手术式写入 ──────────────────────────────────────────────────────────

// specPatchParts 在**原始字节**上只替换 `modules[i].parts` 的值。
//
// 不解析成结构再序列化的三个理由(全部在文件头展开):不丢未知字段、不丢键序与
// 缩进、不给"整包写"任何可乘之机。做不到的情形一律**报错不写**(模块没有 parts
// 键、modules 不是数组…)—— 半改一半的 spec 比没改的 spec 危险得多。
//
// want 的键是模块 name(大小写敏感,与文件里写的一致)。
func specPatchParts(src []byte, want map[string][]string) ([]byte, error) {
	if len(want) == 0 {
		return src, nil
	}
	type edit struct {
		start, end int
		repl       []byte
	}
	var edits []edit
	remaining := map[string]bool{}
	for k := range want {
		remaining[k] = true
	}

	dec := json.NewDecoder(bytes.NewReader(src))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("spec 不是合法 JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("spec 顶层不是对象")
	}
	for dec.More() {
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return nil, kerr
		}
		key, _ := keyTok.(string)
		if key != "modules" {
			var skip json.RawMessage
			if serr := dec.Decode(&skip); serr != nil {
				return nil, serr
			}
			continue
		}
		open, oerr := dec.Token()
		if oerr != nil {
			return nil, oerr
		}
		if d, ok := open.(json.Delim); !ok || d != '[' {
			return nil, fmt.Errorf("spec 的 modules 不是数组 —— 拒绝回填")
		}
		for dec.More() {
			var raw json.RawMessage
			if derr := dec.Decode(&raw); derr != nil {
				return nil, derr
			}
			end := int(dec.InputOffset())
			start := end - len(raw)
			var head struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &head) != nil {
				continue
			}
			parts, ok := want[strings.TrimSpace(head.Name)]
			if !ok {
				continue
			}
			ps, pe, perr := specPartsValueSpan(raw)
			if perr != nil {
				return nil, fmt.Errorf("模块 %s: %w", head.Name, perr)
			}
			repl, merr := json.Marshal(parts)
			if merr != nil {
				return nil, merr
			}
			edits = append(edits, edit{start: start + ps, end: start + pe, repl: repl})
			delete(remaining, strings.TrimSpace(head.Name))
		}
		if _, cerr := dec.Token(); cerr != nil { // 吃掉 ']'
			return nil, cerr
		}
	}
	if len(remaining) > 0 {
		names := make([]string, 0, len(remaining))
		for k := range remaining {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("在 spec 的 modules 里找不到模块 %s —— 拒绝写入(半改的 spec 比没改的更危险)",
			strings.Join(names, ", "))
	}
	// 从后往前拼接,前面的偏移才不会被前一次替换挪动。
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), src...)
	for _, e := range edits {
		out = append(out[:e.start], append(append([]byte(nil), e.repl...), out[e.end:]...)...)
	}
	return out, nil
}

// specPartsValueSpan 在一个模块对象的原始字节里定位 `parts` 值的 [start,end)。
// 没有 parts 键时报错 —— 插入一个新键会破坏原有缩进风格,而"写得难看"在一份
// 手写的设计正本上是真代价;让用户先补一个空 `"parts": []` 再回填更诚实。
func specPartsValueSpan(obj json.RawMessage) (int, int, error) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, fmt.Errorf("模块不是对象")
	}
	for dec.More() {
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return 0, 0, kerr
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if derr := dec.Decode(&raw); derr != nil {
			return 0, 0, derr
		}
		if key == "parts" {
			end := int(dec.InputOffset())
			return end - len(raw), end, nil
		}
	}
	return 0, 0, fmt.Errorf("没有 parts 键 —— 先在 spec 里补一个 \"parts\": [],再重跑回填")
}
