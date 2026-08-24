package app

// cmd_workflow_pages.go — `easyeda workflow pages`:工程状态里逐页记账的归属表。
//
// ## 它解决的是什么
//
// 状态文件按**工程名**分文件,而名字会被同名重建复用(测试工程 `ceshi` 删掉重建
// 三次,每次 uuid 都变、名字都叫 ceshi)。页级读取对此免疫,**跨页读取**(spec
// 位号回填)会把死工程的组一并收进分母,写出一份画布上不存在的位号表且零报错。
//
// 修法是把身份放进数据里(internal/workflow/identity.go):每页记账盖工程 uuid
// 戳,跨页读取按活体 uuid 收窄。本命令是这套机制的**人的接口**:
//
//	pages           看逐页归属(own / foreign / unknown)+ 各页记账量
//	pages --reap    拿活体页表核销一次(需要连着窗口):在页表里的盖戳,
//	                不在的标成外来 —— 旧格式(一页戳都没有)唯一的出路
//	pages --prune   删掉已证明外来的那些页的记账(**唯一的删除入口**)
//
// ## 为什么删除只走显式命令
//
// 「证明外来」只降级成「不参与匹配」,数据一个字节不动。自动清会在工程只是换了
// 个团队空间(uuid 变了、活儿还是那些活儿)时吃掉真状态 —— 那种错误不可逆,而
// 多留几页死记账只是噪音。噪音可以每次报出来提醒,数据没了就没了。

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

func newWorkflowPagesCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var asJSON, reap, prune bool
	c := &cobra.Command{
		Use:   "pages",
		Short: "逐页记账的工程归属表(--reap 用活体页表核销,--prune 删掉已证明外来的残留)",
		Long: `列出这份工程状态里每一页记账(虚拟组 / 区认领 / 区框 id)属于哪个工程。

状态文件按工程**名**分文件,而同名重建(删掉再建一个同名工程)会让几个不同
工程的页记账堆进同一个文件。页级读取不受影响(死工程的 documentUuid 配不上
活页),但**跨页**读取(spec 位号回填、分区打分的分母)会把死页一起收进去,
写出画布上根本不存在的位号表 —— 而且全程零报错。

判定按每页的工程 uuid 归属戳:

  own      归属戳 == 当前窗口的工程 uuid   → 正常参与
  foreign  归属戳是别的 uuid(或已核销为外来)→ **不参与**跨页匹配
  unknown  没有归属戳(升级前写的旧记账)   → 照常参与(证不出外来就不误伤)

--reap 需要一个连着的窗口:拿 schematic.pages.list 的真实页表核销一次 ——
在页表里的页盖上当前工程的戳,不在的标成外来。这是 unknown 变 own/foreign 的
唯一途径,也就是旧格式文件的迁移动作。**空的页表会被拒绝**(读失败也是零页,
拿它当证据等于用一次抖动毁掉整份记账)。

--prune 删掉已判为 foreign 的那些页的全部记账。这是唯一的删除入口 —— 判定层
永远只收窄不删除。`,
		Args: cobra.NoArgs,
		Example: `  easyeda workflow pages --project ceshi
  easyeda workflow pages --project ceshi --reap
  easyeda workflow pages --project ceshi --prune`,
		RunE: func(cmd *cobra.Command, args []string) error {
			project, liveUUID, rerr := resolveStageIdentity(cfg, *window)
			if rerr != nil {
				// 离线也要能看:退回 --project 敲的名字,uuid 交给文件里记的那个。
				project = strings.TrimSpace(cfg.project)
				if project == "" {
					return fmt.Errorf("认不出工程:%w(加 --project <工程名>)", rerr)
				}
			}
			st, err := loadPcbStageState(project)
			if err != nil {
				return err
			}
			bind := st.Bind(liveUUID)
			scope := st.ScopeUUID(liveUUID)

			// 活体页表:--reap 必需;不 reap 时能读到就顺带标一列 live。
			var livePages []string
			var liveErr error
			if reap || liveUUID != "" {
				livePages, liveErr = fetchLiveSchematicPages(cfg, *window)
			}

			var reaped, marked []string
			if reap {
				if liveErr != nil {
					return fmt.Errorf("--reap 需要一个连着的窗口来读真实页表:%w", liveErr)
				}
				var refused bool
				reaped, marked, refused = st.AdoptLivePages(liveUUID, livePages)
				if refused {
					return fmt.Errorf("--reap 拒绝执行:%s —— 空的页表不是证据(读失败也是零页),"+
						"确认窗口打开了这个工程的原理图再重试",
						map[bool]string{true: "认不出活体工程 uuid", false: "活体页表为空"}[strings.TrimSpace(liveUUID) == ""])
				}
				scope = st.ScopeUUID(liveUUID)
			}

			records := st.PageRecords(scope, livePages)
			var foreign []string
			for _, r := range records {
				if r.Verdict == "foreign" {
					foreign = append(foreign, r.UUID)
				}
			}

			var removed []string
			if prune {
				if len(foreign) == 0 {
					return fmt.Errorf("没有已证明属于别的工程的页可清 —— " +
						"旧格式记账(unknown)先跑一次 `--reap` 拿活体页表核销,才有判定依据")
				}
				removed = st.PrunePages(foreign)
			}
			if reap || prune {
				if err := savePcbStageState(st); err != nil {
					return err
				}
			}

			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"project":       project,
					"projectUuid":   st.ProjectUUID,
					"scopeUuid":     scope,
					"pages":         records,
					"reaped":        reaped,
					"markedForeign": marked,
					"pruned":        removed,
					"rebuilt":       bind.Rebuilt,
				})
			}

			fmt.Fprintf(stdout, "workflow pages — project %q (uuid %s)\n", project, shortUUID(st.ProjectUUID))
			if len(records) == 0 {
				fmt.Fprintln(stdout, "  (这份状态里没有任何页记账)")
				return nil
			}
			for _, r := range records {
				mark := map[string]string{"own": "●", "foreign": "✗", "unknown": "?"}[r.Verdict]
				live := ""
				if len(livePages) > 0 {
					live = " live:no"
					if r.Live {
						live = " live:yes"
					}
				}
				fmt.Fprintf(stdout, "  %s %s  owner=%-14s groups=%d zones=%d frames=%d%s\n",
					mark, r.UUID, shortUUID(r.Owner), r.Groups, r.Zones, r.Frames, live)
			}
			for _, p := range reaped {
				fmt.Fprintf(stdout, "  ✓ 核销:%s 确认属于本工程\n", p)
			}
			for _, p := range marked {
				fmt.Fprintf(stdout, "  ✗ 核销:%s 不在活体页表里 → 标为外来(数据未删)\n", p)
			}
			for _, p := range removed {
				fmt.Fprintf(stdout, "  ␡ 已删除:%s 的全部页记账\n", p)
			}
			if len(foreign) > 0 && !prune {
				fmt.Fprintf(stdout, "\n%d 页已证明属于别的工程 —— **不参与**跨页匹配(spec 回填 / 分区打分)。\n"+
					"数据仍在文件里;确认要清掉:`easyeda workflow pages --project %s --prune`\n", len(foreign), project)
			}
			if n := len(st.UnownedPages()); n > 0 && !reap {
				hint := "`easyeda workflow pages --project " + project + " --reap`(需要连着窗口)"
				fmt.Fprintf(stdout, "\n%d 页没有归属戳(升级前写的旧记账),它们照常参与跨页匹配。\n"+
					"若这个工程被同名删除重建过,用 %s 拿真实页表核销一次。\n", n, hint)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "结构化输出")
	c.Flags().BoolVar(&reap, "reap", false, "拿活体页表核销归属(需要连着的窗口);不删任何数据")
	c.Flags().BoolVar(&prune, "prune", false, "删掉已证明属于别的工程的那些页的记账(唯一的删除入口)")
	return c
}

// fetchLiveSchematicPages 读这个窗口所属工程的真实原理图页 uuid 列表。
// 空列表与读失败在调用方那里必须被当成同一回事:都不构成「这些页是死页」的证据。
func fetchLiveSchematicPages(cfg *appConfig, window string) ([]string, error) {
	res, err := requestAction(cfg, "schematic.pages.list", window, nil)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		if u = strings.TrimSpace(u); u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	if arr, ok := res.Result["pages"].([]any); ok {
		for _, it := range arr {
			if m, _ := it.(map[string]any); m != nil {
				add(asString(m["uuid"]))
			}
		}
	}
	// schematics[].page[] 是同一份数据的另一种投影 —— 两边都收,免得某个 build
	// 只填其中一个(少收一页 = 把活页误判成死页,代价比多收一页大得多)。
	if arr, ok := res.Result["schematics"].([]any); ok {
		for _, it := range arr {
			m, _ := it.(map[string]any)
			if m == nil {
				continue
			}
			pgs, _ := m["page"].([]any)
			for _, p := range pgs {
				if pm, _ := p.(map[string]any); pm != nil {
					add(asString(pm["uuid"]))
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func shortUUID(u string) string {
	u = strings.TrimSpace(u)
	switch {
	case u == "":
		return "(无)"
	case u == workflow.ForeignOwner:
		return "(外来)"
	case len(u) > 12:
		return u[:12] + "…"
	}
	return u
}
