package app

// sch_page_fit_test.go — 「这一页放不下它」判据的自带测试(#181 第三份复盘)。
//
// 三件事必须钉住,少一件这个判据就会退化回它要根治的那个病:
//   1. **一把尺**:page-too-small 与 fitsAroundCorner 逐例一致(项目复发过两次的
//      「两把尺」病:同一页 zone-plan 说装不下、sheet tidy 说是排布问题);
//   2. **三档分得开**:装得下/位置不对/尺寸就超了,出路完全不同;
//   3. **建议可执行**:page-too-small 必须先说死「再挪没用」再给出路 —— 少了这
//      半句,读的人(尤其 agent)会继续微调,那正是 8 轮的来源。

import (
	"math/rand"
	"strings"
	"testing"
)

// a4Usable 复刻真机口径:A4 横放 1170×825,可用区 = 内缩 sheetEdgeMinGap。
func a4Usable() layoutBBox {
	return schUsableArea(layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825})
}

// a4Keepout 是真机 A4 图签 keep-out(cmd_sch_sheet.go 的定尺寸口径:702×198 贴右下)。
func a4Keepout() *layoutBBox {
	ko, _ := titleBlockKeepout(&layoutBBox{MinX: 0, MinY: 0, MaxX: 1170, MaxY: 825})
	return ko
}

// boxAt 造一个左下角在 (x,y)、尺寸 w×h 的框。
func boxAt(x, y, w, h float64) layoutBBox {
	return layoutBBox{MinX: x, MinY: y, MaxX: x + w, MaxY: y + h}
}

// TestSchPageFit_SharesRulerWithFitsAroundCorner 是「一把尺」的机械保证:
// judgeSchPageFit 判 page-too-small 当且仅当 fitsAroundCorner 说放不下。
// 随机扫一片尺寸(含各种退化情形),任何一例不一致都说明有人抄了第二份不等式。
func TestSchPageFit_SharesRulerWithFitsAroundCorner(t *testing.T) {
	usable := a4Usable()
	ko := a4Keepout()
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		w := rng.Float64() * 1400
		h := rng.Float64() * 1000
		for _, keepout := range []*layoutBBox{ko, nil} {
			f := judgeSchPageFitSize("x", w, h, true, boxAt(usable.MinX, usable.MinY, w, h), usable, keepout)
			want := !fitsAroundCorner(w, h, usable, keepout)
			if got := f.TooBig(); got != want {
				t.Fatalf("w=%.1f h=%.1f keepout=%v: TooBig=%v, fitsAroundCorner says fits=%v",
					w, h, keepout != nil, got, !want)
			}
		}
	}
}

// TestSchPageFit_Tiers 钉住三档的边界。数字取自 #181 复盘与 WROOM 真机取证。
func TestSchPageFit_Tiers(t *testing.T) {
	usable := a4Usable() // 1146 × 801
	ko := a4Keepout()    // [468,0]-[1170,198] → 左侧净宽 456,上方净高 615

	cases := []struct {
		name string
		box  layoutBBox
		want string
	}{
		{
			// 复盘的主角:legacy 块真实高 820 > 整幅可用高 801。挪多少次都没用。
			name: "legacy 块超高",
			box:  boxAt(50, 20, 300, 820),
			want: schFitTooBig,
		},
		{
			// WROOM 那一档:507×712 —— 宽高各自都小于整幅,但 L 形两条通道都不满足
			// (左侧要 ≤456、上方要 ≤615)。这一档最容易被误判成「挪一挪」。
			name: "L 形两条通道都不满足",
			box:  boxAt(50, 20, 507, 712),
			want: schFitTooBig,
		},
		{
			// 尺寸放得下(能塞进图签左侧的窄长条),只是现在压在图签上 —— 可挪。
			name: "尺寸够但压着图签",
			box:  boxAt(600, 20, 400, 700),
			want: schFitNeedsMove,
		},
		{
			name: "探出纸边",
			box:  boxAt(900, 400, 400, 300),
			want: schFitNeedsMove,
		},
		{
			name: "干净",
			box:  boxAt(50, 300, 400, 400),
			want: schFitOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := judgeSchPageFit("BLK", tc.box, usable, ko)
			if f.Verdict != tc.want {
				t.Fatalf("verdict=%q advice=%q, want %q", f.Verdict, f.Advice, tc.want)
			}
			if tc.want != schFitOK && strings.TrimSpace(f.Advice) == "" {
				t.Fatalf("verdict %q 没给建议 —— 判据的价值在于报出能执行的下一步", f.Verdict)
			}
			if tc.want == schFitOK && f.Advice != "" {
				t.Fatalf("fits 不该带建议,got %q", f.Advice)
			}
		})
	}
}

// TestSchPageFit_TooBigAdviceKillsTheRetryLoop 钉住措辞里那半句要命的话:
// 「再挪/再压 --per-row 都不会变」。它才是把 8 轮砍成 1 轮的那一句;掉了它,
// 剩下的三条出路会被当成"可选的其中一种尝试"。
func TestSchPageFit_TooBigAdviceKillsTheRetryLoop(t *testing.T) {
	f := judgeSchPageFit("MCU", boxAt(50, 20, 300, 820), a4Usable(), a4Keepout())
	if !f.TooBig() {
		t.Fatalf("verdict=%q, want page-too-small", f.Verdict)
	}
	for _, want := range []string{"再挪", "--per-row", "独占一页", "page-new", "实测"} {
		if !strings.Contains(f.Advice, want) {
			t.Fatalf("advice 少了 %q:\n%s", want, f.Advice)
		}
	}
	// 差多少必须报出来 —— 「放不下」三个字不可执行。820 − 801 = 19。
	if !strings.Contains(f.Advice, "820") || !strings.Contains(f.Advice, "801") {
		t.Fatalf("advice 没报出实测高/可用高:\n%s", f.Advice)
	}
}

// TestSchPageFitFromEstimate_OnlyDisproves 钉住估算口径的边界:它只配证否。
// 估算的半高在 bapBlockRect 里是写死的下限,拿它下"放得下"的结论就是把 700 高的
// 块算成 100 高 —— 正是这次要根治的病。
func TestSchPageFitFromEstimate_OnlyDisproves(t *testing.T) {
	usable := a4Usable()
	ko := a4Keepout()

	big := schPageFitFromEstimate("BLK", 1400, 200, usable, ko)
	if !big.TooBig() {
		t.Fatalf("估算宽 1400 > 可用宽 %.0f 却没判 page-too-small", usable.MaxX-usable.MinX)
	}
	if big.Measured {
		t.Fatal("估算口径不许自称 measured")
	}
	if !strings.Contains(big.Advice, "估算") {
		t.Fatalf("估算档的措辞必须写明是估算:\n%s", big.Advice)
	}

	// 放得下时**不下乐观结论**:没有位置信息,fits 只是"没证伪",不带建议。
	small := schPageFitFromEstimate("BLK", 200, 200, usable, ko)
	if small.Verdict != schFitOK || small.Advice != "" {
		t.Fatalf("verdict=%q advice=%q, want fits + 无建议", small.Verdict, small.Advice)
	}
}

// TestSchCornerChannels_NoKeepout:没有图签(隐藏/读不到)时两条通道退化成整幅,
// 与 fitsAroundCorner 的 nil 分支同一语义。
func TestSchCornerChannels_NoKeepout(t *testing.T) {
	usable := a4Usable()
	uw, uh := bboxSize(usable)
	leftW, aboveH := schCornerChannels(usable, nil)
	if leftW != uw || aboveH != uh {
		t.Fatalf("leftW=%.0f aboveH=%.0f, want %.0f/%.0f", leftW, aboveH, uw, uh)
	}
}
