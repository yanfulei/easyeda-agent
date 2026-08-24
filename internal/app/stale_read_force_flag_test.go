package app

// stale_read_force_flag_test.go — 人工逃生口 --force-stale-read 的离线回归,
// 外加「拒绝消息里的每条指令都必须真实存在」的机械钉子。
//
// 起因是一个自打脸的 bug:STALE_READ 门的全部价值在于「判据必须给能执行的下一步」,
// 而它给出的绕过方式 `--force-reason` 是一个**从未注册过**的 flag —— 主干那条
// `easyeda doc reload` 是真的,括号里那句是假的。修法有两半:
//
//	(1) 真把逃生口做出来(--force-stale-read),且不能是第二个 cfg.forceReason;
//	(2) 让「消息里出现的命令/flag」和「真实注册的命令/flag」对不上就转红,
//	    这样以后改消息不会再飘 —— 见 TestRefusalMessagesOnlyNameRealCLISurface。
//
// 本文件自带假 daemon(不复用别处的),断言的是端到端行为:门真的响、flag 真的
// 把读放过去、理由真的落到线上(daemon 靠它写审计行)、而写动作一个都不许被武装。

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// ── 假 daemon:只做 STALE_READ 门这一件事 ─────────────────────────────────

type forceFlagCall struct {
	Action      string
	ForceReason string
}

type forceFlagDaemon struct {
	mu       sync.Mutex
	calls    []forceFlagCall
	stale    string // 非空 = 门关着
	refusals int
}

// 本测试自己的「哪些动作写画布」表:刻意手写,不从目录派生 —— 若两边都从同一份
// 目录生成,表错了也测不出来(和 stale_read_optin_gate_test.go 同一个理由)。
var forceFlagMutates = map[string]bool{
	"pcb.component.modify": true,
	"pcb.line.create":      true,
	"pcb.import_changes":   true,
}

func newForceFlagDaemon(t *testing.T) (*appConfig, *forceFlagDaemon) {
	t.Helper()
	d := &forceFlagDaemon{}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"service":"easyeda-agent","status":"ok","windows":[{"windowId":"w1","context":{"projectName":"forceflag"}}]}`)
	})
	mux.HandleFunc("/action", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action      string         `json:"action"`
			Payload     map[string]any `json:"payload"`
			ForceReason string         `json:"forceReason"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		d.mu.Lock()
		d.calls = append(d.calls, forceFlagCall{Action: req.Action, ForceReason: req.ForceReason})
		dry, _ := req.Payload["dryRun"].(bool)
		isPcb := strings.HasPrefix(req.Action, "pcb.")
		mutates := isPcb && forceFlagMutates[req.Action] && !dry
		if isPcb && !mutates && d.stale != "" && strings.TrimSpace(req.ForceReason) == "" {
			d.refusals++
			mutation := d.stale
			d.mu.Unlock()
			fmt.Fprintf(w, `{"ok":false,"error":{"code":"STALE_READ","message":%q}}`,
				req.Action+" —— PCB 自 "+mutation+" 后未 reload")
			return
		}
		if mutates {
			d.stale = req.Action
		}
		d.mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u := strings.TrimPrefix(srv.URL, "http://")
	i := strings.LastIndex(u, ":")
	if i < 0 {
		t.Fatalf("bad test server url %q", srv.URL)
	}
	return &appConfig{host: u[:i], ports: u[i+1:] + "-" + u[i+1:], project: "forceflag"}, d
}

func (d *forceFlagDaemon) forcedFor(action string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.calls) - 1; i >= 0; i-- {
		if d.calls[i].Action == action {
			return d.calls[i].ForceReason, true
		}
	}
	return "", false
}

// captureStaleReadWarnings 把人工放行提示接到 buffer 上,并清空「每动作一次」的
// 去重表 —— 否则同一进程里跑两个用例,第二个就看不到提示了。
func captureStaleReadWarnings(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := staleReadWarnOut
	staleReadWarnOut = &buf
	staleReadWarnMu.Lock()
	prevSeen := staleReadWarnSeen
	staleReadWarnSeen = map[string]bool{}
	staleReadWarnMu.Unlock()
	t.Cleanup(func() {
		staleReadWarnOut = prev
		staleReadWarnMu.Lock()
		staleReadWarnSeen = prevSeen
		staleReadWarnMu.Unlock()
	})
	return &buf
}

// ── 1. flag 真的把读放过去,且入审计的理由带得上 ───────────────────────────

func TestForceStaleReadFlagLetsTheReadThrough(t *testing.T) {
	warn := captureStaleReadWarnings(t)
	cfg, d := newForceFlagDaemon(t)
	d.mu.Lock()
	d.stale = "pcb.import_changes"
	d.mu.Unlock()

	// 负对照先跑:没有 flag 一定被拒。绿灯若不是因为 flag,这条会先炸。
	if _, err := requestAction(cfg, "pcb.components.list", "w1", nil); !isStaleRead(err) {
		t.Fatalf("门没拦下未放行的 PCB 读 —— 测试装置本身坏了: %v", err)
	}

	cfg.forceStaleRead = "e2e回归:验证绕过路径"
	if _, err := requestAction(cfg, "pcb.components.list", "w1", nil); err != nil {
		t.Fatalf("--force-stale-read 没能把读放过去: %v", err)
	}
	reason, ok := d.forcedFor("pcb.components.list")
	if !ok || reason == "" {
		t.Fatal("放行理由没落到线上 —— daemon 就写不出 daemon.stale_read.force 审计行")
	}
	if !strings.HasPrefix(reason, staleReadPrefixManual) {
		t.Fatalf("人工放行缺少可 grep 的前缀(审计里分不出人工/回读): %q", reason)
	}
	if !strings.Contains(reason, "e2e回归:验证绕过路径") {
		t.Fatalf("用户写的理由被丢了: %q", reason)
	}
	if !strings.Contains(warn.String(), "--force-stale-read") {
		t.Fatalf("整进程生效的放行必须在 stderr 留痕,得到: %q", warn.String())
	}
}

// 一次放行不许静默:提示每动作打一次,但不同动作都要打(去重口径不能宽到
// 「整条命令只提醒一次」)。
func TestForceStaleReadWarnsOncePerAction(t *testing.T) {
	warn := captureStaleReadWarnings(t)
	cfg := &appConfig{forceStaleRead: "看看旧状态"}
	for i := 0; i < 3; i++ {
		staleReadForceReason(cfg, "pcb.components.list", nil)
	}
	staleReadForceReason(cfg, "pcb.nets.list", nil)
	if got := strings.Count(warn.String(), "pcb.components.list"); got != 1 {
		t.Fatalf("同一动作提示了 %d 次,want 1(复合命令会把提示刷成噪音)", got)
	}
	if got := strings.Count(warn.String(), "pcb.nets.list"); got != 1 {
		t.Fatalf("另一个动作没提示(去重太宽): %q", warn.String())
	}
}

// ── 2. 硬约束:它不是第二个 cfg.forceReason ───────────────────────────────

// 人工 flag 必须过和代码放行位同一张收窄表:PCB 域 + 不写画布 + 不受布线阶段门
// 管辖。否则 `pcb autoroute --force-stale-read "x"` 就顺手把布线门也开了。
func TestForceStaleReadNeverArmsWritesOrTheRouteGate(t *testing.T) {
	captureStaleReadWarnings(t)
	cfg := &appConfig{forceStaleRead: "人工理由"}
	for _, a := range []string{
		"pcb.component.modify", "pcb.line.create", "pcb.via.create",
		"pcb.import_autoroute", "pcb.import_changes", "schematic.components.list",
	} {
		if got := staleReadForceReason(cfg, a, map[string]any{"dryRun": true}); got != "" {
			t.Fatalf("%s 被人工 flag 武装了 forceReason=%q —— 写动作/布线门/非 PCB 域都不该沾", a, got)
		}
	}
	if got := staleReadForceReason(cfg, "pcb.drc.check", nil); got == "" {
		t.Fatal("PCB 读应当被人工 flag 放行")
	}
}

// 它也绝不能碰 cfg.forceReason —— 那是布线阶段门的钥匙,共用一个字段就是安全倒退。
func TestForceStaleReadDoesNotTouchForceReason(t *testing.T) {
	captureStaleReadWarnings(t)
	cfg, d := newForceFlagDaemon(t)
	cfg.forceStaleRead = "只放 STALE_READ"
	if _, err := requestAction(cfg, "pcb.components.list", "w1", nil); err != nil {
		t.Fatalf("read: %v", err)
	}
	if cfg.forceReason != "" {
		t.Fatalf("--force-stale-read 污染了 cfg.forceReason=%q —— 会解锁布线阶段门", cfg.forceReason)
	}
	// 线上也不许出现 forceUnsafe(那是 #132 的越权位)。
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) == 0 {
		t.Fatal("没有请求到达假 daemon")
	}
}

// 代码侧的写后回读放行位优先于人工 flag:两者都成立时,审计里该留下更具体的那条。
func TestAfterWriteReasonWinsOverManualFlag(t *testing.T) {
	captureStaleReadWarnings(t)
	cfg := staleReadOptIn(&appConfig{forceStaleRead: "人工理由"}, "刚写完的那批器件")
	got := staleReadForceReason(cfg, "pcb.components.list", nil)
	if !strings.HasPrefix(got, staleReadPrefixAfterWrite) {
		t.Fatalf("写后回读理由被人工 flag 盖掉了: %q", got)
	}
}

// 门拦得住的,flag 必须放得开:daemon 拦的是「PCB 域 + 目录 Mutates=false」的读,
// 所以每一个这样的动作都必须在 staleReadEligible 里 —— 否则消息承诺的绕过对那些
// 动作又是一句空话(两把尺不一致,老毛病)。
func TestEveryGateBlockableReadIsForceable(t *testing.T) {
	for _, a := range protocol.AllActions() {
		if a.Domain != protocol.DomainPcb || a.Mutates {
			continue
		}
		if !staleReadEligible[a.Name] {
			t.Errorf("%s 会被 STALE_READ 门拦下,却带不上放行位(RequiresGate=%q)", a.Name, a.RequiresGate)
		}
	}
}

// ── 3. 钉子:拒绝消息里出现的命令/flag 必须真的注册过 ──────────────────────

// 这就是本轮 bug 的根因防线。它不看代码「有没有写 flag」,而是把 STALE_READ 门
// 相关文件里**所有字符串字面量**扫一遍,把里面出现的 `easyeda <子命令…>` 和
// `--flag` 拿去和真实 Cobra 树比对 —— 消息里再出现不存在的指令就转红。
//
// 扫字面量而不是扫渲染结果,是为了连**还没被任何用例渲染过**的消息也一起管住;
// 注释不参与(go/ast 只给字面量),所以文档性的旧名字不会误伤。
func TestRefusalMessagesOnlyNameRealCLISurface(t *testing.T) {
	files := []string{
		"../daemon/stalereads.go", // 门与拒绝消息
		"stale_read_optin.go",     // app 侧补充的下一步 / 放行提示
	}
	root := newRootCmd(io.Discard, io.Discard)

	cmdRe := regexp.MustCompile("easyeda(?: [a-z][a-z0-9-]*)+")
	flagRe := regexp.MustCompile(`--[a-z][a-z0-9-]*`)

	for _, f := range files {
		lits := stringLiterals(t, f)
		if len(lits) == 0 {
			t.Fatalf("%s: 一个字符串字面量都没扫到 —— 文件被挪走/改名了,这道钉子已经失效", f)
		}
		for _, lit := range lits {
			for _, cmdPath := range cmdRe.FindAllString(lit, -1) {
				assertCommandExists(t, root, f, lit, cmdPath)
			}
			for _, flag := range flagRe.FindAllString(lit, -1) {
				assertFlagExists(t, root, f, lit, flag)
			}
		}
	}
}

// assertCommandExists 解析 "easyeda doc reload" 这样的路径。
func assertCommandExists(t *testing.T, root *cobra.Command, file, lit, cmdPath string) {
	t.Helper()
	args := strings.Fields(cmdPath)[1:] // 去掉 "easyeda"
	found, rest, err := root.Find(args)
	if err != nil || found == nil || len(rest) > 0 {
		t.Errorf("%s 的消息里写着 `%s`,但 CLI 里没有这条命令(未消化: %v)\n  出处: %q",
			file, cmdPath, rest, lit)
	}
}

// assertFlagExists 只要求这个 flag 在命令树的某处注册过 —— 根 persistent 或任一
// 子命令的本地 flag。消息不带上下文,能定位到「哪条命令上有」就够洗清「根本不存在」。
func assertFlagExists(t *testing.T, root *cobra.Command, file, lit, flag string) {
	t.Helper()
	name := strings.TrimPrefix(flag, "--")
	if registeredFlags(root)[name] {
		return
	}
	all := make([]string, 0, len(registeredFlags(root)))
	for f := range registeredFlags(root) {
		all = append(all, "--"+f)
	}
	sort.Strings(all)
	t.Errorf("%s 的消息里让用户加 `%s`,但整棵 CLI 树都没有这个 flag —— 一道给不出可执行下一步的门。\n  出处: %q\n  现有: %s",
		file, flag, lit, strings.Join(all, " "))
}

var (
	registeredFlagsOnce sync.Once
	registeredFlagsSet  map[string]bool
)

func registeredFlags(root *cobra.Command) map[string]bool {
	registeredFlagsOnce.Do(func() {
		registeredFlagsSet = map[string]bool{}
		var walk func(c *cobra.Command)
		walk = func(c *cobra.Command) {
			c.Flags().VisitAll(func(f *pflag.Flag) { registeredFlagsSet[f.Name] = true })
			c.PersistentFlags().VisitAll(func(f *pflag.Flag) { registeredFlagsSet[f.Name] = true })
			for _, sub := range c.Commands() {
				walk(sub)
			}
		}
		walk(root)
	})
	return registeredFlagsSet
}

// stringLiterals 返回一个 Go 文件里全部字符串字面量的解码值(注释不算)。
func stringLiterals(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(bl.Value); err == nil {
			out = append(out, s)
		}
		return true
	})
	return out
}
