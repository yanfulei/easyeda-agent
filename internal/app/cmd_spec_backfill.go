package app

// cmd_spec_backfill.go — `easyeda spec backfill`:把落地后的真实位号写回 S0 spec。
// 纯核与「为什么必须外科手术式写入」在 spec_backfill.go。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

func newSpecBackfillCmd(cfg *appConfig, stdout, stderr io.Writer) *cobra.Command {
	var project, window string
	var write, asJSON bool
	c := &cobra.Command{
		Use:   "backfill <s0-spec.json>",
		Short: "把 block-apply 落地后的真实位号回填进 spec 的 modules[].parts(离线)",
		Long: `S0 spec 里的 modules[].parts 是**designator 字符串**,而 designator 会在落地时
被 EasyEDA 重编号:block-apply 计划 C1、平台按它自己知道的全局位号给出 C11
(它看得见我们预扫描看不见的未加载页,issue #144),我们照实回读并 remap。
画布是 C11、虚拟组记的是 C11 —— 只有手写的 spec 还是 C1。

位号对不上**不会报错**,只会让 ` + "`pcb zones set --spec`" + `、partition 打分、连接器
规则静默少算一个模块,报告照样绿。本命令消除那次手工同步。

事实来源是 workflow 状态里的**持久虚拟组**(block-apply 归组时写入,位号取的正是
remap 之后的真值),所以整条命令**完全离线** —— 不需要连接器,不需要打开 EasyEDA。

匹配规则(声明优先于猜测):
  1. 模块写了 ` + "`block`" + ` → 按块 id 认它的虚拟组(含全部功能子群);
  2. 没写 block → 组名末段 == 模块的 ` + "`zone`" + ` 或 ` + "`name`" + `。
同一个块被两个模块声明 = 歧义,两个都跳过并报出来(不替你做设计决定)。

写入是**外科手术**:只替换 modules[i].parts 那一段字节,键序/缩进/未知字段一个
都不动(整包 Unmarshal→Marshal 会静默丢掉它们 —— 见 attrs_backfill 那次 166 个
位号被库占位灌成 C? 的事故)。任何一个模块定位不到就整体拒绝写入。

默认只预览(dry-run),--write 才落盘。`,
		Example: `  easyeda spec backfill .easyeda/s0-ceshi.json --project ceshi
  easyeda spec backfill .easyeda/s0-ceshi.json --project ceshi --write
  easyeda spec backfill .easyeda/s0-ceshi.json --window a149acb3-... --write   # 同名多窗口时
  easyeda spec backfill s0.json --project ceshi --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(project) == "" {
				project = cfg.project
			}
			// 只有显式给了 --window 才去问实时窗口 —— 这条命令的最大优点是**完全
			// 离线**,不该因为没写 --project 就悄悄要求一个跑着的 daemon。
			// 反查走 resolveStageIdentity:与 block-apply 归组写状态时同一个函数、
			// 同一套优先级,所以两条路径永远落在同一个 state 文件上;它顺带把活体
			// 工程 uuid 带回来,跨页组表因此能按「活的那个工程」收窄。
			// 没有 --window 的纯离线路径拿不到 uuid,退回状态文件里记的 ProjectUUID。
			var liveUUID string
			if strings.TrimSpace(project) == "" && strings.TrimSpace(window) != "" {
				p, u, err := resolveStageIdentity(cfg, window)
				if err != nil {
					return fmt.Errorf("--window %s 认不出工程:%w(改用 --project <工程名>)", window, err)
				}
				project, liveUUID = p, u
				fmt.Fprintf(stderr, "工程 %s(由 --window %s 反查)\n", project, window)
			}
			res, patched, err := runSpecBackfill(args[0], project, liveUUID)
			if err != nil {
				return err
			}
			if write && len(res.Changes) > 0 {
				if err := specWriteAtomic(args[0], patched); err != nil {
					return err
				}
				res.Written = true
			}
			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			renderSpecBackfill(res, write, stdout)
			return nil
		},
	}
	c.Flags().StringVar(&project, "project", "", "工程名(默认取全局 --project);决定读哪份 workflow 状态里的虚拟组")
	c.Flags().StringVar(&window, "window", "", "EasyEDA 窗口 ID;没给 --project 时用它反查工程名(同名多窗口时唯一可用的路由方式)")
	c.Flags().BoolVar(&write, "write", false, "真的写回文件(默认只预览)")
	c.Flags().BoolVar(&asJSON, "json", false, "结构化输出")
	return c
}

// bapBackfillSpec 是 `sch block-apply --spec` 的钩子:落块+归组之后,把这一页的
// 真实位号同步回 S0 spec。**永不返回错误** —— 见调用点注释(器件已落地,外部
// json 没同步不该判整单失败),但每一种失败都必须说出来。
//
// 工程标识由调用方**从响应信封里**带进来(schPageIdentity),不能直接读
// cfg.project:`--window` 是一等的路由方式(同名多窗口时 `--project` 会被 dispatch
// 直接拒掉 —— "maps to 2 windows — pass --window <id>",那时 --window 是唯一能跑
// 的路),原先直接取 cfg.project 等于让回填在那种场合整个失效。
//
// 键的优先级与 resolveStageProject 一致(--project 字符串 > friendlyName/name >
// uuid),所以**两条路径落在同一个 state 文件上** —— 回填读的组表必然就是
// bapRegisterGroup 刚写进去的那一份,不会因为反查方式不同而分裂出第二份。
//
// ident 还带着**活体工程 uuid**:跨页组表按它收窄,同名重建残留的死页因此不进
// 回填的分母(见 spec_backfill.go 的 specCollectGroups)。ident 为空时退回
// resolveStageProject 再问一次窗口 —— 但正常路径零额外往返(信封里本来就有)。
func bapBackfillSpec(cfg *appConfig, window, path string, ident schPageIdentity, stderr io.Writer) {
	project, liveUUID := strings.TrimSpace(ident.Project), strings.TrimSpace(ident.UUID)
	if project == "" {
		// 调用方没能从信封里认出工程(极少见:连响应都没拿到)—— 再问一次窗口。
		p, perr := resolveStageProject(cfg, window)
		if perr != nil {
			fmt.Fprintf(stderr, "warn: spec 位号回填跳过(认不出这块画布属于哪个工程:%v)—— %s\n",
				perr, specBackfillManualHint(path, ""))
			return
		}
		project = p
	}
	res, patched, err := runSpecBackfill(path, project, liveUUID)
	if err != nil {
		fmt.Fprintf(stderr, "warn: spec 位号回填跳过(%v)—— %s\n",
			err, specBackfillManualHint(path, project))
		return
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(stderr, "warn: spec 回填:%s\n", w)
	}
	if len(res.Changes) == 0 {
		fmt.Fprintf(stderr, "spec ✓ %s 的 modules[].parts 与落地位号一致\n", path)
		return
	}
	if err := specWriteAtomic(path, patched); err != nil {
		fmt.Fprintf(stderr, "warn: spec 位号回填写入失败(%v)—— %s\n",
			err, specBackfillManualHint(path, project))
		return
	}
	for _, ch := range res.Changes {
		fmt.Fprintf(stderr, "spec ✓ %s.parts:%s → %s\n",
			ch.Module, strings.Join(ch.Before, " "), strings.Join(ch.After, " "))
	}
}

// runSpecBackfill 读 spec + 工程状态,算出回填并返回打好补丁的字节(未落盘)。
//
// liveUUID 是活体工程 uuid(在线调用方传,离线传空):它决定跨页组表按谁收窄,
// 是「同名重建的死工程组不再进回填分母」的唯一判据。
func runSpecBackfill(path, project, liveUUID string) (specBackfillResult, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return specBackfillResult{}, nil, fmt.Errorf("read spec: %w", err)
	}
	s, err := spec.Parse(raw)
	if err != nil {
		return specBackfillResult{}, nil, err
	}
	if strings.TrimSpace(project) == "" {
		return specBackfillResult{}, nil, fmt.Errorf(
			"回填要知道读哪个工程的虚拟组表 —— 加 --project <工程名>(与 `easyeda sch block-apply --project` 同一个名字)," +
				"或者加 --window <id> 让它反查(同名多窗口时只能走这条)")
	}
	st, err := workflow.Load(project)
	if err != nil {
		return specBackfillResult{}, nil, fmt.Errorf("读工程状态 %s: %w", workflow.Path(project), err)
	}
	// 跨页收集按**活体工程 uuid** 收窄。liveUUID 由调用方给(在线路径能拿到);
	// 纯离线的 CLI 拿不到,就退回文件自己记的 ProjectUUID —— 「最后绑定的那个
	// 工程」同样能把带戳的死页挡在外面,而且一次往返都不用。
	scope := st.ScopeUUID(liveUUID)
	groups, skipped := specCollectGroups(st, scope)
	want, res := specPlanBackfill(s, groups)
	res.Spec, res.Project = path, project
	if len(skipped) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"跳过 %d 页:它们的虚拟组属于另一个工程(同名重建的残留,uuid ≠ %s;页 %s)——"+
				"没有被算进回填的分母。要清掉残留:`easyeda workflow pages --project %s --prune`",
			len(skipped), scope, strings.Join(skipped, ", "), project))
	}
	if len(groups) == 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"工程 %q 的状态里一个虚拟组都没有(%s)—— 要么还没跑过 `sch block-apply`,"+
				"要么 --project 写的不是同一个名字。没有事实来源就没有回填,画布与 spec 都未改动。",
			project, workflow.Path(project)))
		return res, raw, nil
	}
	patched := raw
	if len(want) > 0 {
		if patched, err = specPatchParts(raw, want); err != nil {
			return res, nil, err
		}
	}
	return res, patched, nil
}

// specWriteAtomic 先写临时文件再 rename —— 中途失败不会留下半个 spec。
func specWriteAtomic(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func renderSpecBackfill(res specBackfillResult, write bool, w io.Writer) {
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "WARN  %s\n", warn)
	}
	for _, ch := range res.Changes {
		fmt.Fprintf(w, "%-16s %s\n", ch.Module, strings.Join(ch.Groups, "+"))
		fmt.Fprintf(w, "  before: %s\n", strings.Join(ch.Before, " "))
		fmt.Fprintf(w, "  after:  %s\n", strings.Join(ch.After, " "))
		if len(ch.Added) > 0 {
			fmt.Fprintf(w, "  +%s\n", strings.Join(ch.Added, " "))
		}
		if len(ch.Removed) > 0 {
			fmt.Fprintf(w, "  -%s\n", strings.Join(ch.Removed, " "))
		}
	}
	if len(res.Unchanged) > 0 {
		fmt.Fprintf(w, "unchanged: %s\n", strings.Join(res.Unchanged, ", "))
	}
	if len(res.Unmatched) > 0 {
		// 报出来而不是沉默:漏掉一个模块与写错位号的后果完全一样(判据静默少算)。
		fmt.Fprintf(w, "unmatched(没有对应虚拟组,未改动): %s\n", strings.Join(res.Unmatched, ", "))
		fmt.Fprintf(w, "  —— 手工连线的模块本来就没有组;块驱动的模块请给它写上 `block` 或让组名末段等于 zone/name\n")
	}
	switch {
	case len(res.Changes) == 0:
		fmt.Fprintln(w, "✓ spec 的 modules[].parts 与落地位号一致,无需回填")
	case res.Written:
		fmt.Fprintf(w, "✓ 已回填 %d 个模块 → %s\n", len(res.Changes), res.Spec)
	case write:
		fmt.Fprintln(w, "(未写入)")
	default:
		fmt.Fprintf(w, "%d 个模块位号已漂移 —— 只预览,加 --write 落盘\n", len(res.Changes))
	}
}
