package app

// spec_backfill_window_test.go — 回填的**路由**这一维:`--window` 也必须能回填,
// 而且必须落在与 `--project` 完全相同的那一个 state 文件上。
//
// 真机现场(esp32Mini 端到端):双连接器 → `--project ceshi` 被 dispatch 拒掉
// ("maps to 2 windows — pass --window <id>"),于是只能用 `--window`;而回填直接
// 读 cfg.project,空字符串 → 整条功能静默跳过,提示语里还是 `--project  --write`
// (占位符没填,抄下来必然跑不通)。
//
// 本文件自带 mock daemon,不碰任何共享 helper。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// sbwDaemon 是一个最小 daemon 替身:/health 报一个窗口,/action 按动作名回话,
// 并**逐条记录调用**(「--project 时一次 project.current 都不该发」是本文件的
// 判据之一,只有记下来才验得了)。
type sbwDaemon struct {
	mu      sync.Mutex
	actions []string
	// projectResp 是 project.current 的回话;空 = 回一个失败(模拟认不出工程)。
	projectResp string
}

func (d *sbwDaemon) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.actions...)
}

func (d *sbwDaemon) called(action string) bool {
	for _, a := range d.calls() {
		if a == action {
			return true
		}
	}
	return false
}

func newSbwDaemon(t *testing.T, projectResp string) (*appConfig, *sbwDaemon) {
	t.Helper()
	d := &sbwDaemon{projectResp: projectResp}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"service":"easyeda-agent","windows":[{"windowId":"w1"}]}`))
		case "/action":
			var body struct {
				Action string `json:"action"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			d.mu.Lock()
			d.actions = append(d.actions, body.Action)
			d.mu.Unlock()
			if body.Action == "project.current" {
				if d.projectResp == "" {
					_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"EDA_CALL_FAILED","message":"No current project is open."}}`))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":` + d.projectResp + `}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portText, _ := strings.Cut(hostPort, ":")
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse test daemon port: %v", err)
	}
	return &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port)}, d
}

// sbwSpec 是一份最小 spec:MCU 声明了块,位号却停在计划值 C1/U1(真机上平台会
// 把它们重编成 C11/U3 —— 这正是回填要消掉的漂移)。
const sbwSpec = `{
  "board": "compact",
  "modules": [
    {
      "name": "MCU",
      "block": "esp32s3_wroom1_module",
      "parts": ["U1", "C1"]
    }
  ]
}
`

// sbwSetupState 在临时 workflow 目录里放一份「刚被 block-apply 归好组」的状态。
func sbwSetupState(t *testing.T, project string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(workflow.EnvDir, dir)
	st := &workflow.State{Project: project}
	st.SetGroupsForPage("doc1", []*workflow.Group{{
		ID: "g1", Name: "esp32s3_wroom1_module(U3)",
		BlockID: "block.esp32s3_wroom1_module",
		Members: []string{"U3", "C11"},
	}})
	if err := workflow.Save(st); err != nil {
		t.Fatal(err)
	}
	return dir
}

func sbwWriteSpec(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s0.json")
	if err := os.WriteFile(path, []byte(sbwSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sbwParts(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Modules []struct {
			Parts []string `json:"parts"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("回填后的 spec 不是合法 JSON:%v\n%s", err, raw)
	}
	if len(probe.Modules) == 0 {
		t.Fatalf("模块没了:\n%s", raw)
	}
	return probe.Modules[0].Parts
}

// TestBapBackfillSpec_WindowRouteBackfills 是这次修复的正判据:只给 --window
// (cfg.project 为空)也必须真的回填,而不是打一行 warn 就跳过。
func TestBapBackfillSpec_WindowRouteBackfills(t *testing.T) {
	sbwSetupState(t, "ceshi")
	path := sbwWriteSpec(t)
	cfg, d := newSbwDaemon(t, `{"uuid":"66458be3-x","name":"ceshi","friendlyName":"ceshi"}`)

	var errBuf strings.Builder
	bapBackfillSpec(cfg, "w1", path, schPageIdentity{}, &errBuf)

	if got := strings.Join(sbwParts(t, path), ","); got != "C11,U3" {
		t.Fatalf("--window 路由没有真的回填,parts=%v\nstderr:\n%s", got, errBuf.String())
	}
	if !d.called("project.current") {
		t.Fatalf("没有从窗口反查工程标识,调用序:%v", d.calls())
	}
	if strings.Contains(errBuf.String(), "跳过") {
		t.Fatalf("仍在跳过:\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "spec ✓") {
		t.Fatalf("回填没有把改动说出来:\n%s", errBuf.String())
	}
}

// TestBapBackfillSpec_WindowAndProjectResolveSameKey 钉的是那个**必须避开的坑**:
// resolveStageProject 优先用 --project 敲的字符串做文件名,所以从 windowId 反查
// 出来的标识必须与它一致,否则同一个工程会写出两个 state 文件(那比不回填更糟)。
//
// 判据不是"两个字符串相等"这种自证式断言,而是**文件系统上只出现一个 state 文件**
// 且两条路径都能读到同一份组表。
func TestBapBackfillSpec_WindowAndProjectResolveSameKey(t *testing.T) {
	dir := sbwSetupState(t, "ceshi")

	// ① --window 路由(cfg.project 为空):走反查。
	pathWin := sbwWriteSpec(t)
	cfgWin, dWin := newSbwDaemon(t, `{"uuid":"66458be3-x","name":"ceshi","friendlyName":"ceshi"}`)
	var winErr strings.Builder
	bapBackfillSpec(cfgWin, "w1", pathWin, schPageIdentity{}, &winErr)

	// ② --project 路由:cfg.project 直接给名字。
	pathProj := sbwWriteSpec(t)
	cfgProj, dProj := newSbwDaemon(t, `{"uuid":"66458be3-x","name":"ceshi","friendlyName":"ceshi"}`)
	cfgProj.project = "ceshi"
	var projErr strings.Builder
	bapBackfillSpec(cfgProj, "", pathProj, schPageIdentity{}, &projErr)

	winParts := strings.Join(sbwParts(t, pathWin), ",")
	projParts := strings.Join(sbwParts(t, pathProj), ",")
	if winParts != projParts || winParts != "C11,U3" {
		t.Fatalf("两条路由回填出不同结果:window=%s project=%s\nwindow stderr:\n%s\nproject stderr:\n%s",
			winParts, projParts, winErr.String(), projErr.String())
	}

	// --project 给了字符串就不该再问窗口 —— 多一次往返事小,**换一个标识**事大。
	if dProj.called("project.current") {
		t.Fatalf("--project 路径仍去问了窗口:%v", dProj.calls())
	}
	if !dWin.called("project.current") {
		t.Fatalf("--window 路径没问窗口:%v", dWin.calls())
	}

	// 文件系统判据:整个 workflow 目录里只有 ceshi.json 一个 state 文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != filepath.Base(workflow.Path("ceshi")) {
		t.Fatalf("两条路由分裂出了不同的 state 文件:%v(期望只有 %s)",
			names, filepath.Base(workflow.Path("ceshi")))
	}
}

// TestBapBackfillSpec_UnresolvableProjectHintIsRunnable:反查失败时不许再吐出
// `--project  --write` 这种抄下来跑不通的命令。
func TestBapBackfillSpec_UnresolvableProjectHintIsRunnable(t *testing.T) {
	sbwSetupState(t, "ceshi")
	path := sbwWriteSpec(t)
	cfg, _ := newSbwDaemon(t, "") // project.current 直接失败

	var errBuf strings.Builder
	bapBackfillSpec(cfg, "w1", path, schPageIdentity{}, &errBuf)
	out := errBuf.String()

	if strings.Contains(out, "--project  ") || strings.Contains(out, "--project --write") {
		t.Fatalf("空占位符又回来了:\n%s", out)
	}
	if !strings.Contains(out, "project info") && !strings.Contains(out, "health") {
		t.Fatalf("没说清怎样才能拿到工程名:\n%s", out)
	}
	// 画布已经落地,回填失败只能是 warn,不能升级成错误(函数无返回值,这里钉措辞)。
	if !strings.Contains(out, "warn:") {
		t.Fatalf("回填失败应当降级成一行 warn:\n%s", out)
	}
}

// TestSpecBackfillManualHint_NeverEmitsEmptyPlaceholder 是同一条判据的纯函数版:
// 提示语存在的唯一理由是给人照抄,拼不出真名字就必须换一种说法。
func TestSpecBackfillManualHint_NeverEmitsEmptyPlaceholder(t *testing.T) {
	withName := specBackfillManualHint(".easyeda/s0.json", "ceshi")
	if !strings.Contains(withName, "--project ceshi --write") {
		t.Fatalf("有工程名时该给出可直接照抄的命令:%s", withName)
	}
	for _, project := range []string{"", "   "} {
		got := specBackfillManualHint(".easyeda/s0.json", project)
		if strings.Contains(got, "--project  ") || strings.Contains(got, "--project --write") ||
			strings.HasSuffix(strings.TrimSpace(got), "--project") {
			t.Fatalf("project=%q 时拼出了空占位符:%s", project, got)
		}
		if !strings.Contains(got, "<工程名>") || !strings.Contains(got, "--window") {
			t.Fatalf("project=%q 时没给出获取工程名的办法:%s", project, got)
		}
	}
}
