package app

import (
	"encoding/json"
	"fmt"
	"io"
)

// ── phase A 失败也要给得出诊断 ────────────────────────────────────────────────
//
// phase A 一挂,过去就只剩一行错误,而且 `--json` **连 JSON 都不出**(错误在
// asJSON 分支之前就 return 了)。2026-08-26 实测:POWER 页 J2 反复被 R5 拒,
// 照着那一行反复试了 4 轮也定位不到是哪支端子、走的哪条支路 —— 判据拦得住,
// 却没给人任何可以着手的东西。
//
// 现在失败带上**这一区每支端子的 kind / net / side / pin 坐标 / 有无实测引脚**:
// 「有没有实测引脚」尤其关键,它决定了走让位支路还是首版兜底支路,而两条支路
// 对同侧标签的处理完全不同。

// zaDiagTerm 是一支端子的诊断快照。
type zaDiagTerm struct {
	Kind   string  `json:"kind"`
	Net    string  `json:"net"`
	Side   string  `json:"side"`
	PinX   float64 `json:"pinX"`
	PinY   float64 `json:"pinY"`
	HasPin bool    `json:"hasPin"` // false = 没有实测引脚,走首版兜底支路(不让位)
}

// zaDiagGroup 是一个组的诊断快照。
type zaDiagGroup struct {
	Designator string       `json:"designator"`
	BodyW      float64      `json:"bodyW"`
	BodyH      float64      `json:"bodyH"`
	MultiPin   bool         `json:"multiPin"`
	Terms      []zaDiagTerm `json:"terms"`
}

// zaDiagGroups 折出一批组的诊断快照(纯函数)。
func zaDiagGroups(groups []zfGroup) []zaDiagGroup {
	out := make([]zaDiagGroup, 0, len(groups))
	for _, g := range groups {
		d := zaDiagGroup{Designator: g.Designator, BodyW: g.BodyW, BodyH: g.BodyH, MultiPin: g.MultiPin}
		for _, t := range g.Terms {
			d.Terms = append(d.Terms, zaDiagTerm{
				Kind: t.Kind, Net: t.Net, Side: t.Side,
				PinX: t.PinX, PinY: t.PinY, HasPin: t.HasPin,
			})
		}
		out = append(out, d)
	}
	return out
}

// zoneArrangePhaseAError 是带诊断的 phase A 失败。
type zoneArrangePhaseAError struct {
	Zone   string
	Err    error
	Groups []zaDiagGroup
}

func (e *zoneArrangePhaseAError) Error() string {
	return fmt.Sprintf("phase A(%s): %v", e.Zone, e.Err)
}

func (e *zoneArrangePhaseAError) Unwrap() error { return e.Err }

// writeJSON 把失败连同诊断序列化 —— `--json` 承诺的是「机器可读」,
// 失败路径尤其需要:成功时人还能看输出,失败时人只有这一份。
func (e *zoneArrangePhaseAError) writeJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"ok":      false,
		"verdict": "blocked",
		"phase":   "A",
		"zone":    e.Zone,
		"error":   e.Err.Error(),
		"groups":  e.Groups,
		"hint": "看每支端子的 hasPin:false = 这一组没有实测引脚坐标,走的是首版兜底支路" +
			"(同侧标签不让位),同侧两支旗必然叠 —— 先确认这些引脚真的连上了" +
			"(`sch list --include-pins` / `sch bridge-check`)。true 则走让位支路," +
			"叠了说明让位没收敛,把 side/pinY 贴出来报 issue。",
	})
}

// zaDiagText 是 phase A 失败的人读诊断(stderr)。--json 之外的调用方也该看得到
// 「是哪支端子、走的哪条支路」,而不是只有一行 R5。
func zaDiagText(w io.Writer, e *zoneArrangePhaseAError) {
	fmt.Fprintf(w, "诊断 —— 区 %q 的端子明细(hasPin=false 表示没有实测引脚,走不让位的兜底支路):\n", e.Zone)
	for _, g := range e.Groups {
		fmt.Fprintf(w, "  %s 本体 %.0f×%.0f multiPin=%v\n", g.Designator, g.BodyW, g.BodyH, g.MultiPin)
		for _, t := range g.Terms {
			fmt.Fprintf(w, "    %-8s %-10s side=%-6s pin=(%.1f,%.1f) hasPin=%v\n",
				t.Kind, t.Net, t.Side, t.PinX, t.PinY, t.HasPin)
		}
	}
}
