package workflow

// identity.go — 「这份状态到底属于哪个工程」的身份层。
//
// ## 病historie
//
// 状态文件按**工程名**分文件(Dir()/<SanitizeKey(project)>.json),而
// resolveStageProject 又刻意让人敲的 `--project` 字符串赢过一切(否则人敲得出的
// 名字定位不到自己的状态)。这两条合在一起有一个静默后果:**同名重建**——测试
// 工程 `ceshi` 删掉重建三次,每次 uuid 都变、名字都叫 ceshi,于是四个不同工程的
// 页记账全堆进同一个 ceshi.json(实测 7 页里 4 页属于已删工程)。
//
// 页级读取(GroupsForPage(docUUID))对此免疫:死工程的 documentUuid 永远配不上活
// 页。**真正中毒的是跨页读取** —— specCollectGroups 故意跨页收集(模块搬页不能
// 漏),于是死页的组正常参与匹配,回填写出一份画布上根本不存在的位号表,全程零
// 报错。污染的是判据的分母。
//
// ## 修法:身份放进数据里,不让文件名去背
//
// 文件名继续按名字(不改 resolveStageProject 的优先级),但**每一页记账都盖上写
// 它的那个工程的 uuid**(PageOwners),整份文件也记住最后绑定的 uuid
// (ProjectUUID)。跨页读取时按活体 uuid 收窄:证明是别人的 → 不参与;证不出来的
// (旧格式无戳)→ 照常参与,直到被活体页表核销一次(AdoptLivePages)。
//
// 三条铁律:
//
//   - **绝不自动删**。证明外来只影响「参不参与匹配」,数据一个字节不动;删除只走
//     用户显式的 `easyeda workflow pages --prune`。自动清会在「工程只是换了个团队
//     空间(uuid 变了)」时吃掉真状态。
//   - **证不出来就不收窄**。没戳的页(升级前写的)默认参与 —— 一个用了半年的真实
//     工程不该因为升级了一个版本就丢掉五页事实来源。
//   - **空的活体页表不构成证据**。读页表失败 = 零页,若据此把所有页标成外来,就
//     等于用一次网络抖动毁掉整份记账。AdoptLivePages 对空列表直接拒绝。

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ForeignOwner 是「已证明不属于当前绑定工程,但真实归属未知」的所有者标记。
// 它只由 AdoptLivePages 写入(拿活体页表核销旧格式记账时),不是一个 uuid ——
// uuid 都是十六进制,"-" 与之天然不冲突。
const ForeignOwner = "-"

// BindResult 是一次身份绑定的结论。零值(以及 Bind 传空 uuid)表示「什么都没发生」。
type BindResult struct {
	// LiveUUID 是这次绑定到的活体工程 uuid。
	LiveUUID string
	// PriorUUID 是绑定之前文件里记的 uuid(旧格式为空)。
	PriorUUID string
	// Rebuilt = 文件此前绑过**另一个** uuid。同名重建的直接证据。
	Rebuilt bool
	// LegacyUnstamped = 文件里已经有页记账,却一页都没有归属戳(升级前写的)。
	// 此时无法判断里面有没有死工程的页 —— 需要一次 AdoptLivePages 核销。
	LegacyUnstamped bool
	// Foreign 是已证明不属于 LiveUUID 的页(带戳且戳不对,或标了 ForeignOwner)。
	Foreign []string
	// Unowned 是没有归属戳的页。
	Unowned []string
}

// NeedsAttention 报告这次绑定是否有值得对人说的事。
func (b BindResult) NeedsAttention() bool {
	return b.Rebuilt || len(b.Foreign) > 0 || b.LegacyUnstamped
}

// Message 是给人看的一段话:说清发现了什么、下一步敲什么。**不含自动动作**。
func (b BindResult) Message(project string) string {
	if !b.NeedsAttention() {
		return ""
	}
	var sb strings.Builder
	switch {
	case b.Rebuilt:
		fmt.Fprintf(&sb, "工程状态 %q 此前绑定的是另一个工程(uuid %s),现在这个窗口是 uuid %s ——"+
			"同名重建过吗?两份记账现在共用一个文件。", project, short(b.PriorUUID), short(b.LiveUUID))
	case len(b.Foreign) > 0:
		fmt.Fprintf(&sb, "工程状态 %q 里有 %d 页属于别的工程(同名重建的残留)。", project, len(b.Foreign))
	default:
		fmt.Fprintf(&sb, "工程状态 %q 是旧格式(页记账没有工程归属戳,共 %d 页)——"+
			"若这个工程被同名删除重建过,旧页仍会参与跨页匹配。", project, len(b.Unowned))
	}
	if len(b.Foreign) > 0 {
		sb.WriteString(fmt.Sprintf("已证明外来的 %d 页**不再参与**跨页匹配(回填/分区),数据仍在文件里。", len(b.Foreign)))
	}
	sb.WriteString("下一步:`easyeda workflow pages --project " + project + "` 看逐页归属;" +
		"确认要清掉残留再加 `--prune`(不加就什么都不删)。")
	return sb.String()
}

func short(uuid string) string {
	if len(uuid) > 12 {
		return uuid[:12] + "…"
	}
	if uuid == "" {
		return "(无)"
	}
	return uuid
}

// Bind 把状态绑定到活体工程 uuid 上。空 uuid = 不知道身份,直接返回零值(离线
// 路径照旧工作,只是不会盖戳、也不会收窄)。
//
// 绑定**只写身份字段**:它会把 ProjectUUID 更新成活体 uuid(最后写入者),并让
// 之后的页写入自动盖上这个戳。它**不动任何页记账** —— 判定与清理彻底分开。
func (s *State) Bind(projectUUID string) BindResult {
	uuid := strings.TrimSpace(projectUUID)
	if s == nil || uuid == "" {
		return BindResult{}
	}
	s.bound = uuid
	res := BindResult{LiveUUID: uuid, PriorUUID: strings.TrimSpace(s.ProjectUUID)}
	res.Foreign = s.ForeignPages(uuid)
	res.Unowned = s.UnownedPages()
	switch {
	case res.PriorUUID == "":
		// 旧格式 / 从未绑过:一页戳都没有时**不做任何归因** —— 没有证据。
		res.LegacyUnstamped = len(res.Unowned) > 0
	case res.PriorUUID != uuid:
		res.Rebuilt = true
		// 这份文件此前绑的是 PriorUUID,所以此刻**还没戳的页只能是它写的** ——
		// 把它们归给 PriorUUID。这一步让「同名重建」这个只在绑定那一刻可见的事实
		// **变成数据**:否则结论只出现一次(下一次 ProjectUUID 已经是新的了),而
		// 死页会继续参与跨页匹配。归因是盖戳,不是删除;`workflow pages` 看得到,
		// 判错了也只影响"参不参与匹配",数据仍在。
		if len(res.Unowned) > 0 {
			if s.PageOwners == nil {
				s.PageOwners = map[string]string{}
			}
			for _, p := range res.Unowned {
				s.PageOwners[p] = res.PriorUUID
			}
			res.Foreign = append(res.Foreign, res.Unowned...)
			sort.Strings(res.Foreign)
			res.Unowned = nil
		}
	}
	if res.Rebuilt {
		s.History = append(s.History, Event{
			Stage: "identity", At: time.Now().Format(time.RFC3339), Action: "rebind",
			Note: fmt.Sprintf("project uuid %s → %s (same name, different project?); %d page(s) attributed to the previous project",
				res.PriorUUID, uuid, len(res.Foreign)),
		})
	}
	s.ProjectUUID = uuid
	return res
}

// BoundUUID 是本次进程里绑定到的活体 uuid(未绑定为空)。
func (s *State) BoundUUID() string {
	if s == nil {
		return ""
	}
	return s.bound
}

// ScopeUUID 决定跨页读取按哪个 uuid 收窄:显式给的活体 uuid 优先,取不到就退回
// 文件自己记的 ProjectUUID(「最后绑定的那个工程」)。两个都没有 = 不收窄。
//
// 退回 ProjectUUID 让**纯离线**的调用方(`easyeda spec backfill --project X`,
// 它的最大优点就是不需要跑着的 daemon)也能把带戳的死页挡在外面。
func (s *State) ScopeUUID(explicit string) string {
	if u := strings.TrimSpace(explicit); u != "" {
		return u
	}
	if s == nil {
		return ""
	}
	return strings.TrimSpace(s.ProjectUUID)
}

// stampPage 给一页盖上当前绑定的工程戳。未绑定(离线)时什么都不做 —— 宁可留
// 「未知」也不要盖一个猜出来的戳,后者会把判据变成谎言。
func (s *State) stampPage(documentUUID string) {
	doc := strings.TrimSpace(documentUUID)
	if s == nil || doc == "" || s.bound == "" {
		return
	}
	if s.PageOwners == nil {
		s.PageOwners = map[string]string{}
	}
	s.PageOwners[doc] = s.bound
}

// forgetPage 清掉一页的归属戳(该页的记账被删干净时调用)。
func (s *State) forgetPage(documentUUID string) {
	if s == nil || s.PageOwners == nil {
		return
	}
	delete(s.PageOwners, documentUUID)
	if len(s.PageOwners) == 0 {
		s.PageOwners = nil
	}
}

// PageOwnerOf 返回一页的归属戳("" = 没戳,归属未知)。
func (s *State) PageOwnerOf(documentUUID string) string {
	if s == nil {
		return ""
	}
	return s.PageOwners[strings.TrimSpace(documentUUID)]
}

// PageInScope 判定一页的记账可否参与**跨页**读取。
//
//	liveUUID == ""        → 全放行(不知道活体身份,没有收窄的依据)
//	戳 == liveUUID        → 放行(证明是自己的)
//	没戳                  → 放行(证不出外来;旧格式默认不误伤)
//	戳 == 别的 uuid / "-" → 拦下(已证明外来)
//
// 页级读取(GroupsForPage 等)不走这条:它们按活体 documentUuid 取,天然不会
// 撞上死工程的页。
func (s *State) PageInScope(documentUUID, liveUUID string) bool {
	if s == nil || strings.TrimSpace(liveUUID) == "" {
		return true
	}
	owner := s.PageOwnerOf(documentUUID)
	if owner == "" {
		return true
	}
	return owner == strings.TrimSpace(liveUUID)
}

// PageKeys 是所有页记账的键并集(排序,同输入同输出)。
func (s *State) PageKeys() []string {
	if s == nil {
		return nil
	}
	set := map[string]bool{}
	for p := range s.GroupsByPage {
		set[p] = true
	}
	for p := range s.SchZonesByPage {
		set[p] = true
	}
	for p := range s.SchZoneFrameIdsByPage {
		set[p] = true
	}
	for p := range s.PageOwners {
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ForeignPages 列出已证明不属于 liveUUID 的页。
func (s *State) ForeignPages(liveUUID string) []string {
	live := strings.TrimSpace(liveUUID)
	if s == nil || live == "" {
		return nil
	}
	var out []string
	for _, p := range s.PageKeys() {
		if owner := s.PageOwnerOf(p); owner != "" && owner != live {
			out = append(out, p)
		}
	}
	return out
}

// UnownedPages 列出没有归属戳的页(旧格式记账)。
func (s *State) UnownedPages() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, p := range s.PageKeys() {
		if s.PageOwnerOf(p) == "" {
			out = append(out, p)
		}
	}
	return out
}

// PageRecord 是一页记账的摘要(`workflow pages` 展示用)。
type PageRecord struct {
	UUID   string `json:"documentUuid"`
	Owner  string `json:"owner,omitempty"`
	Groups int    `json:"groups,omitempty"`
	Zones  int    `json:"zones,omitempty"`
	Frames int    `json:"frames,omitempty"`
	// Verdict:own / foreign / unknown —— 相对给定的活体 uuid。
	Verdict string `json:"verdict"`
	// Live 表示这一页在活体页表里(只有做过核销/带页表时才有意义)。
	Live bool `json:"live,omitempty"`
}

// PageRecords 汇总每一页的记账量与归属判定。livePages 为 nil 时 Live 一律 false。
func (s *State) PageRecords(liveUUID string, livePages []string) []PageRecord {
	if s == nil {
		return nil
	}
	liveSet := map[string]bool{}
	for _, p := range livePages {
		if p = strings.TrimSpace(p); p != "" {
			liveSet[p] = true
		}
	}
	live := strings.TrimSpace(liveUUID)
	var out []PageRecord
	for _, p := range s.PageKeys() {
		rec := PageRecord{
			UUID:   p,
			Owner:  s.PageOwnerOf(p),
			Groups: len(s.GroupsByPage[p]),
			Zones:  len(s.SchZonesByPage[p]),
			Live:   liveSet[p],
		}
		if s.SchZoneFrameIdsByPage[p] != nil {
			rec.Frames = 1
		}
		switch {
		case live == "" || rec.Owner == "":
			rec.Verdict = "unknown"
		case rec.Owner == live:
			rec.Verdict = "own"
		default:
			rec.Verdict = "foreign"
		}
		out = append(out, rec)
	}
	return out
}

// AdoptLivePages 拿**活体页表**核销一次归属:在活体页表里的页盖上 liveUUID 的戳
// (已证明是自己的),不在里面的页标成 ForeignOwner(已证明不是自己的 —— 要么属于
// 同名重建前的那个工程,要么这一页早被删了)。
//
// 这是旧格式(一页戳都没有)唯一的出路:没有活体页表就永远证不出死页是死页。
//
// **不删任何数据**,只改归属戳;删除是 PrunePages 的事,而它只由用户显式命令调用。
//
// 空的 livePages 直接拒绝(返回 refused=true):读页表失败也是零页,拿它当证据
// 就等于用一次读失败毁掉整份记账。
func (s *State) AdoptLivePages(liveUUID string, livePages []string) (stamped, foreign []string, refused bool) {
	live := strings.TrimSpace(liveUUID)
	if s == nil || live == "" {
		return nil, nil, true
	}
	liveSet := map[string]bool{}
	for _, p := range livePages {
		if p = strings.TrimSpace(p); p != "" {
			liveSet[p] = true
		}
	}
	if len(liveSet) == 0 {
		return nil, nil, true
	}
	for _, p := range s.PageKeys() {
		owner := s.PageOwnerOf(p)
		if liveSet[p] {
			if owner != live {
				if s.PageOwners == nil {
					s.PageOwners = map[string]string{}
				}
				s.PageOwners[p] = live
				stamped = append(stamped, p)
			}
			continue
		}
		// 不在活体页表里。已经标过外来 / 已经属于别的 uuid 的不必再动。
		if owner == ForeignOwner || (owner != "" && owner != live) {
			foreign = append(foreign, p)
			continue
		}
		if s.PageOwners == nil {
			s.PageOwners = map[string]string{}
		}
		s.PageOwners[p] = ForeignOwner
		foreign = append(foreign, p)
	}
	if len(stamped) > 0 || len(foreign) > 0 {
		s.History = append(s.History, Event{
			Stage: "identity", At: time.Now().Format(time.RFC3339), Action: "reap",
			Note: fmt.Sprintf("%d page(s) confirmed live, %d marked foreign (project %s)",
				len(stamped), len(foreign), live),
		})
	}
	return stamped, foreign, false
}

// PrunePages 删掉指定页的全部记账(组 / 区认领 / 框 id / 归属戳)。**只由用户显式
// 命令调用** —— 判定层永远只收窄,不删除。返回真正删掉的页。
func (s *State) PrunePages(pages []string) []string {
	if s == nil {
		return nil
	}
	var removed []string
	for _, p := range pages {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		hit := false
		if _, ok := s.GroupsByPage[p]; ok {
			delete(s.GroupsByPage, p)
			hit = true
		}
		if _, ok := s.SchZonesByPage[p]; ok {
			delete(s.SchZonesByPage, p)
			hit = true
		}
		if _, ok := s.SchZoneFrameIdsByPage[p]; ok {
			delete(s.SchZoneFrameIdsByPage, p)
			hit = true
		}
		if _, ok := s.PageOwners[p]; ok {
			hit = true
		}
		s.forgetPage(p)
		if hit {
			removed = append(removed, p)
		}
	}
	if len(s.GroupsByPage) == 0 {
		s.GroupsByPage = nil
	}
	if len(removed) > 0 {
		s.History = append(s.History, Event{
			Stage: "identity", At: time.Now().Format(time.RFC3339), Action: "prune",
			Note: fmt.Sprintf("removed page bookkeeping for %d page(s): %s",
				len(removed), strings.Join(removed, ", ")),
		})
	}
	return removed
}
