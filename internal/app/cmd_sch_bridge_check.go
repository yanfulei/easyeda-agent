package app

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ── sch bridge-check: tree-granularity net-vs-copper consistency gate ────────
//
// `sch check`'s multi-net-wire rule looks at a SINGLE wire primitive at a time,
// but EasyEDA merges two collinear touching stubs of DIFFERENT nets into one
// wire tree spanning several wire primitives — no single wire then carries two
// net names, so the per-wire rule under-reports the short. bridge-check groups
// wires into trees by shared vertices (union-find, connector side) and
// aggregates the net names of the netflag/netport anchored on each tree:
//
//   • len(set(nets)) > 1                     → BRIDGE (real short, ERROR, gate)
//   • nets empty & tree touches a comp pin   → ORPHAN (dangling stub, WARN)
//
// Rule types (kebab-case, same convention as sch check / pcb check findings;
// derived from the connector's Kind enum in parseBridgeReport):
//
//	wire-bridge   ERROR  one wire tree carries ≥2 net names (real short) — gates
//	orphan-stub   WARN   wire tree touches pins but carries no net flag/port
//
// It is the third pillar of the S5 verification gate (network-semantics vs
// physical-copper consistency), alongside layout-lint and check/drc. Read-only:
// it reports each problem tree's wire ids / flag ids / touched pins so the fix
// can be driven by hand (sch prim-delete + sch connect) or a later --repair
// pass. BRIDGE findings drive a non-zero exit so it can gate a workflow.

type bridgeTree struct {
	Kind string `json:"kind"` // BRIDGE | ORPHAN (connector enum, kept for compat)
	// Type/Level are the kebab-case rule name + severity in the same convention
	// as sch check / pcb check findings, derived Go-side from Kind (the connector
	// only sends Kind): BRIDGE → wire-bridge/error, ORPHAN → orphan-stub/warn.
	Type    string   `json:"type,omitempty"`
	Level   string   `json:"level,omitempty"`
	WireIds []string `json:"wireIds"`
	FlagIds []string `json:"flagIds"`
	Pins    []string `json:"pins"` // designator:pin
	Nets    []string `json:"nets"`
}

// bridgeRuleType maps the connector's Kind enum to the kebab-case rule type +
// level used across sch check / pcb check findings.
func bridgeRuleType(kind string) (ruleType, level string) {
	switch strings.ToUpper(kind) {
	case "BRIDGE":
		return "wire-bridge", "error"
	case "ORPHAN":
		return "orphan-stub", "warn"
	case "ORPHAN_FLAG":
		// A netflag/netport attached to NO wire (issue #137): typically left behind
		// when a merged wire was deleted out from under it. Invisible until a new
		// wire passes through the point and silently inherits the stray net name.
		return "orphan-flag", "warn"
	case "ORPHAN_TREE":
		// A wire tree that touches NO pin at all: flag+stub left behind by a
		// component move (live 2026-08-18: two GND flag+stub trees survived C4/SW2
		// moves and neither ORPHAN — needs a touched pin — nor ORPHAN_FLAG — needs
		// the flag to sit on NO wire — could see them), or a bare dead wire tree.
		return "orphan-tree", "warn"
	default:
		return strings.ToLower(kind), "warn"
	}
}

type bridgeSummary struct {
	Trees          int `json:"trees"`
	Bridges        int `json:"bridges"`     // per-type count of wire-bridge trees
	Orphans        int `json:"orphans"`     // per-type count of orphan-stub trees
	OrphanFlags    int `json:"orphanFlags"` // per-type count of orphan-flag findings (flag on no wire)
	OrphanTrees    int `json:"orphanTrees"` // per-type count of orphan-tree findings (tree touching no pin)
	WireTreesTotal int `json:"wireTreesTotal"`
}

type bridgeReport struct {
	Passed  bool          `json:"passed"`
	Summary bridgeSummary `json:"summary"`
	Trees   []bridgeTree  `json:"trees"`
	// ToolProbes 是**工具自己**留下的探测残留(见 sch_tool_probe_residue.go),
	// 已从 Trees / Summary 里摘出:它不是板子的问题,不该让 gate 挂掉,
	// 但必须报出来 —— 静默忽略等于让垃圾永远留在画布上。
	ToolProbes []bridgeTree `json:"toolProbes,omitempty"`
}

// runSchBridgeCheck runs the read-only tree-granularity bridge/orphan detection,
// renders it, and returns a non-zero exit when a BRIDGE (real short) exists so it
// can gate a workflow. ORPHAN findings are WARN and do not gate on their own.
func runSchBridgeCheck(cfg *appConfig, window string, allPages, asJSON bool, stdout, stderr io.Writer) error {
	payload := map[string]any{}
	if allPages {
		payload["allPages"] = true
	}
	res, err := requestAction(cfg, "schematic.bridgeCheck", window, payload)
	if err != nil {
		return err
	}

	rep, perr := parseBridgeReport(res.Result)
	if perr != nil {
		if b, mErr := json.MarshalIndent(res.Result, "", "  "); mErr == nil {
			_, _ = stdout.Write(b)
			fmt.Fprintln(stdout)
		}
		return perr
	}

	if asJSON {
		if err := encodeResultEnvelope(res, rep, stdout); err != nil {
			return err
		}
	} else {
		renderBridgeReport(rep, stdout)
	}

	if rep.Summary.Bridges > 0 {
		return fmt.Errorf("sch bridge-check: %d bridge(s) — real short(s) (net-vs-copper mismatch)", rep.Summary.Bridges)
	}
	return nil
}

func parseBridgeReport(result map[string]any) (bridgeReport, error) {
	var rep bridgeReport
	if result == nil {
		return rep, fmt.Errorf("empty bridge-check result")
	}
	b, err := json.Marshal(result)
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return rep, fmt.Errorf("unexpected bridge-check result shape: %w", err)
	}
	// Stamp the kebab-case rule type + level (the connector only sends Kind), so
	// both --json consumers and the text render can gate/count by rule type the
	// same way sch check / pcb check findings are.
	for i := range rep.Trees {
		if rep.Trees[i].Type == "" || rep.Trees[i].Level == "" {
			ruleType, level := bridgeRuleType(rep.Trees[i].Kind)
			if rep.Trees[i].Type == "" {
				rep.Trees[i].Type = ruleType
			}
			if rep.Trees[i].Level == "" {
				rep.Trees[i].Level = level
			}
		}
	}
	// 工具自己的探测残留不算板子的问题(sch_tool_probe_residue.go):连接器的
	// 旋转探测旗删除失败留在画布上,过去被算成一条 orphan-flag,`sch gate --strict`
	// 当场 FAIL —— 电路完全正确的板子过不了自己的门。摘出来单独报,并重算计数
	// (summary 与 trees 必须是同一份事实)。
	real, probes := splitToolProbeResidue(rep.Trees)
	if len(probes) > 0 {
		rep.Trees = real
		rep.ToolProbes = probes
		rep.Summary = recountBridgeSummary(rep.Summary, real)
		rep.Passed = rep.Summary.Bridges == 0
	}
	return rep, nil
}

// recountBridgeSummary 按摘完探测残留的树重算逐型计数。WireTreesTotal 是画布
// 事实(总树数),不随分类改变 —— 探测旗确实占着一棵树。
func recountBridgeSummary(s bridgeSummary, trees []bridgeTree) bridgeSummary {
	out := bridgeSummary{WireTreesTotal: s.WireTreesTotal, Trees: len(trees)}
	for _, t := range trees {
		switch strings.ToUpper(t.Kind) {
		case "BRIDGE":
			out.Bridges++
		case "ORPHAN":
			out.Orphans++
		case "ORPHAN_FLAG":
			out.OrphanFlags++
		case "ORPHAN_TREE":
			out.OrphanTrees++
		}
	}
	return out
}

func renderBridgeReport(rep bridgeReport, w io.Writer) {
	s := rep.Summary
	fmt.Fprintf(w, "sch bridge-check: %d problem tree(s) — %d wire-bridge(s) (real short), %d orphan-stub(s) (dangling stub), %d orphan-flag(s) (flag on no wire), %d orphan-tree(s) (tree touching no pin) across %d wire tree(s)\n",
		s.Trees, s.Bridges, s.Orphans, s.OrphanFlags, s.OrphanTrees, s.WireTreesTotal)

	for _, t := range rep.Trees {
		ruleType, level := t.Type, t.Level
		if ruleType == "" || level == "" {
			// Trees built directly (not via parseBridgeReport) fall back to Kind.
			rt, lv := bridgeRuleType(t.Kind)
			if ruleType == "" {
				ruleType = rt
			}
			if level == "" {
				level = lv
			}
		}
		// Rule-type column aligned with sch check / pcb check's %-17s style.
		line := fmt.Sprintf("  %-5s  %-17s  %s", strings.ToUpper(level), ruleType, t.Kind)
		if len(t.Nets) > 0 {
			line += "  nets=[" + strings.Join(t.Nets, ",") + "]"
		}
		if len(t.Pins) > 0 {
			line += "  pins=[" + strings.Join(t.Pins, ",") + "]"
		}
		fmt.Fprintln(w, line)
		if len(t.WireIds) > 0 {
			fmt.Fprintf(w, "          wires: %s\n", strings.Join(t.WireIds, ", "))
		}
		if len(t.FlagIds) > 0 {
			fmt.Fprintf(w, "          flags: %s\n", strings.Join(t.FlagIds, ", "))
		}
	}

	// 工具自己的探测残留:不计进上面的账(它不是板子的问题、不该让 gate 挂掉),
	// 但必须报出来并给一条能直接抄去跑的清理命令 —— 静默忽略等于让垃圾长住画布。
	if len(rep.ToolProbes) > 0 {
		ids := toolProbeResidueIDs(rep.ToolProbes)
		fmt.Fprintf(w, "  NOTE   tool-probe-residue  %d 个**工具自己**的探测残留(不计入上面的问题数):"+
			"连接器测旋转语义时造的一次性探测旗没删干净(平台删除会撒谎)。\n", len(rep.ToolProbes))
		fmt.Fprintf(w, "         清掉它:easyeda sch prim-delete --ids %s\n", strings.Join(ids, ","))
	}

	if rep.Passed {
		fmt.Fprintln(w, "✓ no bridges or orphans")
		return
	}
	if s.Bridges > 0 {
		fmt.Fprintln(w, "→ bridge (共线合并短路): delete the whole tree (sch prim-delete <wireIds+flagIds>), then re-connect each pin to its own net (sch connect)")
	}
	if s.Orphans > 0 {
		fmt.Fprintln(w, "→ orphan (孤儿桩): the tree touches pins but carries no net flag/port — wire it to a real net or clear the stray stub (sch disconnect / prim-delete)")
	}
	if s.OrphanFlags > 0 {
		fmt.Fprintln(w, "→ orphan-flag (孤儿标志): a netflag/netport sits on NO wire — delete it (sch prim-delete <flagId>) before a new wire inherits its stray net name")
	}
	if s.OrphanTrees > 0 {
		fmt.Fprintln(w, "→ orphan-tree (悬空树): flag+stub 成树却不触任何引脚(挪件残留)或纯裸死线 — `sch prim-delete <wireIds+flagIds>` 整树清掉")
	}
}
