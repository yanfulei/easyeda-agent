package app

// sch_converge_ledger.go — 「同一个病试了几遍了」的跨命令台账(#181 第三份复盘)。
//
// ## 为什么台账在进程外
//
// 复盘里最贵的一条是「block-apply / --per-row 压缩后重叠 → 手工收敛,**反复 8+ 轮**」,
// 用户给的处方是「自动布局重试加次数限制」。第一反应是去找那个死循环 —— 但**代码里
// 一个都没有**:block-apply 的螺旋是 96 个固定候选、网格扫描是有界穷举、关系求解器
// 的 fitAlong 是 tries≤6、推让明确写着「一遍松弛不需要迭代」、zone-arrange 有
// zaSearchBudget、destagger 有 --max-rounds、move 内核对账最多 2 轮。**每一个循环都有界。**
//
// 那 8 轮在哪?**在人(或 agent)身上。**命令跑完 → 报「3 处重叠」→ 人照着提示挪一挪 /
// 压一压 --per-row → 再跑 → 再报「3 处重叠」。单次调用永远是"有界且尽力"的,而
// 「这已经是第 8 次给出同一句话了」这个事实**没有任何一次调用看得见** —— 进程一退
// 就忘光了。次数限制因此必须落在**进程外**:一个按(命令,页,目标)记账的小台账。
//
// ## 记什么、什么时候升级结论
//
// 只记**失败签名**(sig)。签名相同 = 同一个病没动;签名变了或成功了 = 有进展,
// **计数清零**。这条是台账不误伤的全部依据:一个人真的在把问题往前推(重叠 3→1、
// 换了别的落点、改了朝向),他永远撞不到上限;只有原地打转才会。
//
// 撞到上限时命令**在动手之前停下**并给结论,而不是再跑一遍同样的动作:
//
//	停手:… 已连续 3 次同一个结果(…)。这不是摆放问题,再跑一轮不会有别的结果。
//	下一步二选一:① 让它独占一页 …  ② 继续分页 …
//	(确认还要再试:--max-attempts 0)
//
// ## 不变式
//
//   - **台账坏了绝不挡活**:读不出/解析不了一律当空账(布局工具的可靠性不该依赖
//     一个缓存文件);写失败只警告。
//   - **只在失败路径记账**:成功必须清账,否则下一次正常调用会被上一次的历史误伤。
//   - **上限可关**:--max-attempts 0 = 不限。判据能被关掉,人才不会为了绕过它去
//     删状态文件(那会连带删掉别的东西)。
//   - 实测尺寸随账带走:下一次调用因此能在**动手之前**用**上一轮的实测 bbox** 判
//     「这一页根本放不下」——「只信实测」与「提前停手」这两条本来矛盾(没落地就
//     没有实测),台账是把它们同时满足的那把钥匙。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// schConvergeDefaultMaxAttempts 是默认上限。
//
// 3 不是拍脑袋:第 1 次是老老实实的一次尝试;第 2 次是**照着工具自己给的提示**改完
// 再来(挪落点 / 改 --per-row / 换 --at)—— 这一次必须允许,否则提示就成了摆设;
// 第 3 次仍是同一个签名,说明提示这条路走不通,证据已经够了。再往下每一轮都是纯亏
// (复盘实测:8+ 轮,~40% 活跃时间)。
const schConvergeDefaultMaxAttempts = 3

// schConvergeLedgerVersion 是台账格式版本。版本不认识时整本当空账重来 ——
// 台账是可再生的诊断数据,不是设计数据,不值得为它写迁移。
const schConvergeLedgerVersion = 1

// schConvergeKey 唯一确定一个「收敛目标」。
type schConvergeKey struct {
	// Op 是命令名(block-apply / destagger / group-move / zone-arrange)。
	// 不同命令各记各的:同一页上 block-apply 卡住不该让 group-move 也被拦。
	Op string
	// Page 是文档 uuid(取不到时用页名)。跨页是不同的问题 —— 同一个块在 P1 放不下,
	// 在空白的 P2 上完全可能放得下,那正是「独立成页」这条出路的意义。
	Page string
	// Target 是被折腾的对象:块 id / 组 id / 区名。空字符串合法(整页级操作)。
	Target string
}

func (k schConvergeKey) id() string {
	return strings.Join([]string{
		strings.TrimSpace(k.Op),
		strings.TrimSpace(k.Page),
		strings.TrimSpace(k.Target),
	}, "|")
}

// String 给报错用的人类可读形式。
func (k schConvergeKey) String() string {
	if strings.TrimSpace(k.Target) == "" {
		return k.Op
	}
	return k.Op + " " + k.Target
}

// schConvergeRecord 是一个目标的账页。
type schConvergeRecord struct {
	Op     string `json:"op"`
	Page   string `json:"page,omitempty"`
	Target string `json:"target,omitempty"`
	// Signature 是上一次的失败签名。它变了就说明有进展 —— 计数清零的唯一依据。
	Signature string `json:"signature"`
	Attempts  int    `json:"attempts"`
	FirstAt   string `json:"firstAt,omitempty"`
	LastAt    string `json:"lastAt,omitempty"`
	// Fit 是上一次记账时的**实测**装配诊断(有才带)。下一次调用靠它做落块前的
	// 停手判断 —— 这是台账里唯一"有用到下一轮"的数据,其余都只是计数。
	Fit *schPageFit `json:"fit,omitempty"`
	// LastAdvice 是上一次给出的可执行下一步,撞上限时原样复述(人看到的是同一句话
	// 第 N 次出现,那正是我们要让他意识到的事)。
	LastAdvice string `json:"lastAdvice,omitempty"`
}

// schConvergeLedger 是一个工程的全部账页。
type schConvergeLedger struct {
	Version int                           `json:"version"`
	Records map[string]*schConvergeRecord `json:"records"`

	path string // 不落盘
}

// schConvergeLedgerPath 与 workflow 状态同目录(~/.easyeda-agent/workflow/),
// 但**是独立文件**:台账是可丢弃的诊断数据,不该和工作流阶段门(会被 confirm /
// fingerprint 校验)搅在一个 JSON 里 —— 一方写坏不该拖垮另一方。
func schConvergeLedgerPath(project string) string {
	return filepath.Join(workflow.Dir(), "converge-"+workflow.SanitizeKey(project)+".json")
}

// loadConvergeLedger 读台账。**任何问题都返回空账,永不报错** —— 见文件头不变式。
func loadConvergeLedger(project string) *schConvergeLedger {
	l := &schConvergeLedger{
		Version: schConvergeLedgerVersion,
		Records: map[string]*schConvergeRecord{},
		path:    schConvergeLedgerPath(project),
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return l
	}
	var got schConvergeLedger
	if json.Unmarshal(data, &got) != nil || got.Version != schConvergeLedgerVersion {
		return l
	}
	if got.Records != nil {
		l.Records = got.Records
	}
	return l
}

// save 原子写盘;失败只回错误,由调用方决定是否打印(绝不阻断)。
func (l *schConvergeLedger) save() error {
	if l == nil || l.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// get 取账页(没有则 nil)。
func (l *schConvergeLedger) get(k schConvergeKey) *schConvergeRecord {
	if l == nil {
		return nil
	}
	return l.Records[k.id()]
}

// note 记一次**失败**。签名与上次相同 → 计数 +1;不同 → 视为有进展,从 1 重新起算。
// 返回记账后的次数。
func (l *schConvergeLedger) note(k schConvergeKey, sig string, fit *schPageFit, advice string) int {
	if l == nil {
		return 0
	}
	now := time.Now().Format(time.RFC3339)
	rec := l.Records[k.id()]
	if rec == nil || rec.Signature != sig {
		rec = &schConvergeRecord{
			Op: k.Op, Page: k.Page, Target: k.Target,
			Signature: sig, Attempts: 0, FirstAt: now,
		}
		l.Records[k.id()] = rec
	}
	rec.Attempts++
	rec.LastAt = now
	rec.Fit = fit
	rec.LastAdvice = advice
	return rec.Attempts
}

// clear 销账。成功路径**必须**调它 —— 不销账等于让历史失败去拦未来的正常调用。
func (l *schConvergeLedger) clear(k schConvergeKey) {
	if l == nil {
		return
	}
	delete(l.Records, k.id())
}

// schConvergeStop 是「停手」判决。
type schConvergeStop struct {
	Key      schConvergeKey `json:"-"`
	Attempts int            `json:"attempts"`
	Max      int            `json:"maxAttempts"`
	Sig      string         `json:"signature"`
	Fit      *schPageFit    `json:"fit,omitempty"`
	Advice   string         `json:"advice,omitempty"`
}

// Error 让停手判决可以直接当 error 返回 —— 它就是命令的终态,不是"顺便说一句"。
func (s *schConvergeStop) Error() string { return s.message() }

func (s *schConvergeStop) message() string {
	var b strings.Builder
	fmt.Fprintf(&b, "停手:%s 在本页已连续 %d 次得到同一个结果(%s),达到 --max-attempts=%d。",
		s.Key.String(), s.Attempts, s.Sig, s.Max)
	b.WriteString("**再跑一轮同样的命令不会有别的结果** —— 画布未改动。")
	advice := strings.TrimSpace(s.Advice)
	if advice == "" && s.Fit != nil {
		advice = strings.TrimSpace(s.Fit.Advice)
	}
	if advice != "" {
		b.WriteString("\n下一步:" + advice)
	} else {
		b.WriteString("\n下一步二选一:① 让这个目标独占一页(`easyeda sch page-new --name <页名>` 后在新页重放);" +
			"② 继续分页 —— 把本页其余模块搬走腾出整幅。")
	}
	b.WriteString("\n(确认还要再试一次:加 `--max-attempts 0` 关闭上限;或先做出实质改变——" +
		"改变了结果签名的尝试会自动重新计数。)")
	return b.String()
}

// schConvergeGate 在动手**之前**问一句:这个目标是不是已经原地打转到上限了?
//
// max <= 0 表示不限(调用方把 --max-attempts 0 透传进来)。返回非 nil 时调用方
// **必须在发出任何 mutating action 之前**返回它 —— 停手的价值就在于画布零改动。
func schConvergeGate(project string, k schConvergeKey, max int) *schConvergeStop {
	if max <= 0 {
		return nil
	}
	rec := loadConvergeLedger(project).get(k)
	if rec == nil || rec.Attempts < max {
		return nil
	}
	return &schConvergeStop{
		Key: k, Attempts: rec.Attempts, Max: max,
		Sig: rec.Signature, Fit: rec.Fit, Advice: rec.LastAdvice,
	}
}

// schConvergeNoteFailure 记一次失败并返回记账后的次数(0 = 没记成,不阻断)。
// 一并把「这是第 N/M 次」印到 stderr —— 用户在**撞上限之前**就该看见计数在涨,
// 而不是第 3 次突然被拦住。
func schConvergeNoteFailure(project string, k schConvergeKey, sig string,
	fit *schPageFit, advice string, max int, stderr io.Writer) int {

	// 上限关掉时**连账都不记**:台账唯一的用途就是喂那道门,门不开就没有理由在
	// 用户机器上写文件(单测也因此天然不碰真实 HOME)。
	if max <= 0 {
		return 0
	}
	l := loadConvergeLedger(project)
	n := l.note(k, sig, fit, advice)
	if err := l.save(); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "warn: 收敛台账写入失败(%v)—— 次数限制本轮不生效\n", err)
	}
	if stderr != nil && n > 1 && max > 0 {
		fmt.Fprintf(stderr, "收敛台账:%s 第 %d/%d 次得到同一个结果(%s)—— 到 %d 次将停手并给结论\n",
			k.String(), n, max, sig, max)
	}
	return n
}

// schConvergeNoteSuccess 销账。成功、或**签名变了**(有进展)都该调。
func schConvergeNoteSuccess(project string, k schConvergeKey) {
	l := loadConvergeLedger(project)
	if l.get(k) == nil {
		return // 没有账页就不必写盘
	}
	l.clear(k)
	_ = l.save() // 销账失败最多让下一次多拦一回,不值得打扰用户
}

// schConvergeFitFor 取上一轮记下的**实测**装配诊断(没有则 nil)。
// 落块前的「这一页根本放不下」判断靠它 —— 见文件头最后一条不变式。
func schConvergeFitFor(project string, k schConvergeKey) *schPageFit {
	rec := loadConvergeLedger(project).get(k)
	if rec == nil {
		return nil
	}
	return rec.Fit
}

// schConvergeSignature 把一组「这次结果是什么」的要素折成稳定签名。
//
// 要素必须是**结果的性质**,不是时间戳/id 这类每次都变的东西 —— 签名一变就清零,
// 用了不稳定的要素等于把上限关掉。数值一律先粗化(见调用方),避免 1 个单位的
// 抖动就算"有进展"。
func schConvergeSignature(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return "unknown"
	}
	return strings.Join(kept, ",")
}

// schActionDocUUID 从响应信封里取活页 documentUuid(取不到 = 空串)。
// 台账按页记账要它 —— 同一个毛病在两页上是两个问题,不该互相拦。
func schActionDocUUID(res *actionResult) string {
	if res == nil || res.Context == nil {
		return ""
	}
	return strings.TrimSpace(res.Context.DocumentUUID)
}

// schConvergeRecords 返回排好序的账页(诊断/测试用,同输入同输出)。
func (l *schConvergeLedger) records() []*schConvergeRecord {
	if l == nil {
		return nil
	}
	keys := make([]string, 0, len(l.Records))
	for k := range l.Records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*schConvergeRecord, 0, len(keys))
	for _, k := range keys {
		out = append(out, l.Records[k])
	}
	return out
}
