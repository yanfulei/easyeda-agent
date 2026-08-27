package app

import "strings"

// ── 工具自己的探测残留 ≠ 板子的问题 ──────────────────────────────────────────
//
// 连接器为了测出平台的旋转语义(createNetFlag 在某些 build 上**存储取负**),
// 会在离画布极远处造一支一次性探测旗、读回它的 rotation、再删掉:
//
//	extension/src/actions.ts — detectRotationNegation()
//	  createNetFlag('Power', '__ROTPROBE__', 990000, 990000, 90) → getAll → delete
//
// 那句 delete **没有回读验证**,而「删除撒谎」是平台的已知病(单条也会撒谎);
// 一旦 delete 抛错还会被 catch 吞掉,探测旗就永久留在画布上。
//
// 后果不是电路错,而是**判据被污染**:2026-08-26 实测 bridge-check 把它算成
// 一条 orphan-flag,于是 `sch gate --strict`(S5 逐页门用的就是它)当场 FAIL ——
// 一块电路完全正确的板子,因为工具自己留下的垃圾过不了自己的门。
//
// 这里在 **Go 侧**收口:探测残留单独归类,不计进板子的 orphan 账,但**必须报出来**
// 并给出清理命令 —— 静默忽略等于让垃圾永远留在画布上,那是另一种撒谎。
// 放在 Go 侧的理由:立即生效,且覆盖所有**已经装好的**连接器(含市场版旧版本),
// 不必等用户重装 .eext。连接器侧的 delete 回读验证是治本,两者不冲突。

// schToolProbeNets 是工具自己创建的临时探测网名。
//
// 判据是**精确网名**,不是前缀/包含匹配:用户完全可能给自己的网起名
// `PROBE_5V`,把它误判成工具残留、从判据里摘掉,比多报一条 WARN 危险得多。
var schToolProbeNets = map[string]bool{
	// 连接器的旋转语义探测旗(detectRotationNegation)。
	"__ROTPROBE__": true,
}

// isSchToolProbeNet 报告这个网名是不是工具自己的探测残留。
func isSchToolProbeNet(net string) bool {
	return schToolProbeNets[strings.TrimSpace(net)]
}

// schTreeIsToolProbe 报告一棵 bridge-check 的树是不是纯粹的工具探测残留。
//
// **全部**网名都是探测网才算 —— 只要挂着一个真网名,它就已经和电路有牵连了
// (新线穿过会继承那个真网名),照常按板子的问题报。
func schTreeIsToolProbe(t bridgeTree) bool {
	if len(t.Nets) == 0 {
		return false
	}
	for _, n := range t.Nets {
		if !isSchToolProbeNet(n) {
			return false
		}
	}
	return true
}

// splitToolProbeResidue 把探测残留从问题树里摘出来,返回(真问题, 残留)。
// 计数由调用方按摘完的结果重算 —— summary 与 trees 必须是同一份事实。
func splitToolProbeResidue(trees []bridgeTree) (real, probes []bridgeTree) {
	for _, t := range trees {
		if schTreeIsToolProbe(t) {
			probes = append(probes, t)
			continue
		}
		real = append(real, t)
	}
	return real, probes
}

// toolProbeResidueIDs 收集残留的图元 id —— 报文要给出可直接抄去跑的清理命令。
func toolProbeResidueIDs(probes []bridgeTree) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ids []string) {
		for _, id := range ids {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	for _, t := range probes {
		add(t.FlagIds)
		add(t.WireIds)
	}
	return out
}
