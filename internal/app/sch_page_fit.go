package app

// sch_page_fit.go — 「这一页放不下它」的**唯一判据**(#181 第三份复盘,最大卡点)。
//
// ## 立项现场
//
// 一位独立用户按 esp32MiniRequire 走多页原理图,复盘里排第一的卡点是:
//
//	legacy 块超高放不下(block-apply / --per-row 压缩后重叠 → 手工收敛,反复 8+ 轮)
//	—— 真实高 700–840,A4 横版塞不进;--per-row 压缩必带内部重叠,逐脚修。
//
// 注意这句话里真正致命的部分:**他试了 8 轮**。工具每一轮都如实报了「N 处重叠」
// 或「越出图纸可用区」,而这两句话读起来都像「挪一挪 / 压一压就好」—— 于是他挪、
// 他压、他逐脚修,再跑,再报同样的话。工具从头到尾没说过那句唯一有用的:
// **这块的高度比整页可用高还大,挪多少次都不会变**。
//
// 这就是本文件存在的理由:把「摆得不好」(挪能解)和「装不下」(挪不能解,只能
// 拆块 / 独立成页 / 继续分页)分成两个**不同的结论**,并且让所有报「放不下」的
// 路径共用同一把尺。sch_zone_capacity.go 已经为 zone-plan 做过这件事
// (`diagnoseZoneCapacity` + `fitsAroundCorner`);这里做的是把那把尺**抬出来**给
// 块/虚拟组这一侧用,而不是再写第二把 —— `judgeSchPageFit` 的 `page-too-small`
// 判决**必须**与 `fitsAroundCorner` 逐例一致,由
// TestSchPageFit_SharesRulerWithFitsAroundCorner 钉死(「两把尺」是本项目复发过
// 两次的病,见 memory: two-rulers-and-early-read)。
//
// ## 判据形状:L 形,不是矩形
//
// 可用区 = 图纸内缩 sheetEdgeMinGap;图签占**右下角**(y-UP,见 cmd_sch_sheet.go)。
// 所以一个 w×h 的框有两条活路:
//
//	① 待在图签**左侧**的窄长条:宽 ≤ leftW,高不受限(≤ 整幅高);
//	② 绕到图签**上方**的整幅:宽不受限(≤ 整幅宽),高 ≤ aboveH。
//
// 两条都不成立才是 page-too-small。把它说成「可用区只有 W×H」会让人拿框去比那个
// 矩形、越比越糊涂(框明明比整幅小,凭什么说装不下)—— 措辞必须跟着判据走。
//
// ## 铁律:只信实测 bbox
//
// 本文件只做**判定**,不做测量。传进来的 box 必须是实测渲染 bbox
// (`--include-bbox` / `getPrimitivesBBox` 那条链),绝不是标称/估算。估算口径的
// 半高在 bapBlockRect 里是写死的 bapPartMargin(见那里的注释),用它判 page-too-small
// 会把 700 高的块算成 100 高 —— 正是这次要根治的病。估算只配做**下限**:
// 估算都装不下 = 一定装不下(schPageFitFromEstimate 就是这一档,措辞里写明是估算)。

import "fmt"

// 三态判决。永不输出「大概能放吧」。
const (
	// schFitOK:尺寸与位置都没问题。
	schFitOK = "fits"
	// schFitNeedsMove:**尺寸放得下**,只是当前位置压着图签/探出纸边 —— 挪一挪能解,
	// group-move / zone-arrange 这类命令继续跑是有意义的。
	schFitNeedsMove = "needs-move"
	// schFitTooBig:**尺寸本身就超了**。挪、压 --per-row、调 margin/gutter 全都无效,
	// 出路只有三条:拆块 / 独立成页 / 继续分页。这是本文件要造出来的那句话。
	schFitTooBig = "page-too-small"
)

// schPageFit 是一次「装不装得下」的完整诊断。字段全部无 omitempty —— 读的人要能
// 分清「这一维是 0」和「这版没这个字段」(与 zoneArrangeZoneOut.Retained 同一理由)。
type schPageFit struct {
	// Name 是被判的对象(虚拟组名 / 块 id / 区名),用于报告。
	Name string `json:"name"`
	// W/H 是被判框的尺寸。Measured=true 时它来自实测渲染 bbox(唯一可信档);
	// false 时来自估算,只能证否(估算都放不下 ⇒ 真的放不下),不能证真。
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	Measured bool    `json:"measured"`
	// UsableW/H 是整幅可用区(图纸内缩页边距)。
	UsableW float64 `json:"usableW"`
	UsableH float64 `json:"usableH"`
	// LeftW/AboveH 是 L 形两条通道的净尺寸:图签左侧的净宽、图签上方的净高。
	// 没有图签 keep-out 时两者 = 整幅。
	LeftW  float64 `json:"leftW"`
	AboveH float64 `json:"aboveH"`
	// Verdict ∈ {fits, needs-move, page-too-small}。
	Verdict string `json:"verdict"`
	// Advice 是**能直接执行的下一步**,不是「调调看」。
	Advice string `json:"advice"`
}

// TooBig 是给调用方的谓词糖:这一档意味着「再跑一轮同样的命令没有任何意义」。
func (f schPageFit) TooBig() bool { return f.Verdict == schFitTooBig }

// schUsableArea 是「图纸可用区」的唯一算法:图框内缩 sheetEdgeMinGap。
//
// 这个公式此前被逐字复制到 9 处调用点(block-apply / 块求解器 / live push /
// clusters / off-sheet / gate / group-arrange / group-move / note-place)。收成一个
// 函数是为了让「页边距改了」这种事只改一处;新代码一律走它。
func schUsableArea(sheet layoutBBox) layoutBBox {
	return layoutBBox{
		MinX: sheet.MinX + sheetEdgeMinGap, MinY: sheet.MinY + sheetEdgeMinGap,
		MaxX: sheet.MaxX - sheetEdgeMinGap, MaxY: sheet.MaxY - sheetEdgeMinGap,
	}
}

// schCornerChannels 把 L 形可用域折成两条通道的净尺寸(图签左侧净宽 / 图签上方净高)。
// keepout 为 nil 时两条都退化成整幅 —— 与 fitsAroundCorner 的 nil 分支同一语义。
func schCornerChannels(usable layoutBBox, keepout *layoutBBox) (leftW, aboveH float64) {
	uw, uh := bboxSize(usable)
	if keepout == nil {
		return uw, uh
	}
	leftW = keepout.MinX - usable.MinX
	aboveH = uh - (keepout.MaxY - usable.MinY)
	if leftW < 0 {
		leftW = 0
	}
	if aboveH < 0 {
		aboveH = 0
	}
	return leftW, aboveH
}

// judgeSchPageFit 判一个**实测**框在一页上的处境。
//
// keepout 传**未膨胀**的图签矩形还是膨胀过的(inflatedTitleKeepout),由调用方按
// 语境决定并对结果负责:分区框那一侧要留区名带/说明带的安全带,而虚拟组只是
// 「器件 ∪ 它自己的 marker」、不带任何带,膨胀会让它凭空严格一档。两种传法都走
// 同一个函数本体 —— 这才是「一把尺」的意思(尺是函数,不是常量)。
func judgeSchPageFit(name string, box, usable layoutBBox, keepout *layoutBBox) schPageFit {
	w, h := bboxSize(box)
	return judgeSchPageFitSize(name, w, h, true, box, usable, keepout)
}

// schPageFitFromEstimate 是**估算口径**的同一判决 —— 只在没有实测几何时用
// (dry-run / 落块之前)。它只有一个合法用途:估算都装不下 ⇒ 一定装不下
// (估算是下限,见 bapBlockRect 的半高注释)。所以它**永不**输出 fits/needs-move
// 之外的乐观结论,而 page-too-small 这一档的措辞会写明「按估算」。
func schPageFitFromEstimate(name string, w, h float64, usable layoutBBox, keepout *layoutBBox) schPageFit {
	return judgeSchPageFitSize(name, w, h, false, layoutBBox{}, usable, keepout)
}

// judgeSchPageFitSize 是纯核。box 只在 measured 时参与「位置对不对」的判断;
// 估算口径没有位置可言,尺寸放得下就报 fits(它的职责只是证否)。
func judgeSchPageFitSize(name string, w, h float64, measured bool,
	box, usable layoutBBox, keepout *layoutBBox) schPageFit {

	leftW, aboveH := schCornerChannels(usable, keepout)
	uw, uh := bboxSize(usable)
	f := schPageFit{
		Name: name, W: round2(w), H: round2(h), Measured: measured,
		UsableW: round2(uw), UsableH: round2(uh),
		LeftW: round2(leftW), AboveH: round2(aboveH),
		Verdict: schFitOK,
	}
	// **判决与 fitsAroundCorner 必须同源**:这里直接调它,而不是把那三行不等式
	// 抄一遍。抄一遍就是第二把尺,而两把尺迟早给出两个结论(zone-plan 说「装不下、
	// 换 A3」而 sheet tidy 说「是排布问题」的那次事故,根因就是各判各的)。
	if !fitsAroundCorner(w, h, usable, keepout) {
		f.Verdict = schFitTooBig
		f.Advice = schPageFitAdvice(f)
		return f
	}
	if !measured {
		return f // 估算口径只证否,放得下时不下乐观结论
	}
	// 尺寸装得下 —— 那就只剩「现在摆的位置对不对」。位置不对是**可挪**的,
	// 结论必须与 page-too-small 明确区分,否则又变成一句读起来像"再挪挪"的话。
	inside := boxInside(box, usable)
	clearKeepout := keepout == nil || zaOverlapArea(box, *keepout) <= 0
	if inside && clearKeepout {
		return f
	}
	f.Verdict = schFitNeedsMove
	f.Advice = schPageFitAdvice(f)
	return f
}

// schPageFitAdvice 把判决折成一句**能直接执行**的话。
//
// 措辞铁律(踩过):
//   - page-too-small 必须先说死「再挪/再压都不会变」,再给出路。少了这半句,
//     读的人(尤其是 agent)会继续做无意义的微调 —— 那正是 8 轮的来源;
//   - 必须报出**是哪一维、差多少、跟谁比**。「放不下」三个字不可执行;
//   - **A4-only**(用户裁定):两条出路都在算法域之内,绝不建议换纸
//     (平台也根本没有改图纸尺寸的 API)。
func schPageFitAdvice(f schPageFit) string {
	who := f.Name
	if who == "" {
		who = "这个组"
	}
	switch f.Verdict {
	case schFitNeedsMove:
		return fmt.Sprintf("%s 的尺寸(%.0f×%.0f)在本页放得下(可用 %.0f×%.0f),"+
			"只是**位置**压着纸边或图签 —— 挪一挪就能解:`easyeda sch group-move --group <组> --dx … --dy …`,"+
			"或整页重排 `easyeda sch zone-arrange --apply`。",
			who, f.W, f.H, f.UsableW, f.UsableH)
	case schFitTooBig:
		gauge := "实测"
		if !f.Measured {
			gauge = "按估算(估算只保证下限,真实尺寸只会更大)"
		}
		// 说清是被哪一条通道卡住的:两条通道各自缺多少,取"最接近成功"的那条报。
		needLeft := f.W - f.LeftW   // 走图签左侧还差多少宽
		needAbove := f.H - f.AboveH // 走图签上方还差多少高
		var short string
		switch {
		case f.W > f.UsableW && f.H > f.UsableH:
			short = fmt.Sprintf("宽高都超过整幅可用区(差 %.0f × %.0f)", f.W-f.UsableW, f.H-f.UsableH)
		case f.H > f.UsableH:
			short = fmt.Sprintf("高 %.0f 超过整幅可用高 %.0f(差 %.0f)", f.H, f.UsableH, f.H-f.UsableH)
		case f.W > f.UsableW:
			short = fmt.Sprintf("宽 %.0f 超过整幅可用宽 %.0f(差 %.0f)", f.W, f.UsableW, f.W-f.UsableW)
		case needLeft <= needAbove:
			short = fmt.Sprintf("图签把可用区切成 L 形:走图签左侧要宽 ≤ %.0f(本体 %.0f,差 %.0f),"+
				"走图签上方要高 ≤ %.0f(本体 %.0f,差 %.0f)—— 两条都不满足",
				f.LeftW, f.W, needLeft, f.AboveH, f.H, needAbove)
		default:
			short = fmt.Sprintf("图签把可用区切成 L 形:走图签上方要高 ≤ %.0f(本体 %.0f,差 %.0f),"+
				"走图签左侧要宽 ≤ %.0f(本体 %.0f,差 %.0f)—— 两条都不满足",
				f.AboveH, f.H, needAbove, f.LeftW, f.W, needLeft)
		}
		return fmt.Sprintf("%s %s %.0f×%.0f —— %s。"+
			"**这不是摆放问题:再挪、再压 `--per-row`、再调 margin/gutter 都不会让它变小。**"+
			"三条出路(A4-only,不换纸):"+
			"① 让它独占一页 —— `easyeda sch page-new --name <页名>` 后在新页 `block-apply`;"+
			"② 继续分页 —— 把本页其余模块搬到新页,腾出整幅给它;"+
			"③ 把组收小 —— `easyeda sch clusters` 看「组高 vs 本体高」,"+
			"被自己 marker 撑大的脚用 `sch disconnect --pin X:n` + "+
			"`sch connect --pin X:n --direction left|right` 改标签朝向"+
			"(竖排标签能把组撑高数倍:实测本体 21 高的电容,组高 134,改横向后 58)。",
			who, gauge, f.W, f.H, short)
	}
	return ""
}
