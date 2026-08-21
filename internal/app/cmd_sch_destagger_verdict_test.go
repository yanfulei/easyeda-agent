package app

// cmd_sch_destagger_verdict_test.go — destagger「跑完 ≠ 做完」的判决(#181 第三份复盘)。
//
// 补的是一个**沉默的洞**:此前跑满 --max-rounds 还剩 N 处重叠,与一轮就归零,
// 打印的是同一句「已搬迁 M 个 marker(R 轮)」。读的人分不出"做完了"和"没做完",
// 于是只能再跑一遍 —— 再跑一遍还是同一句话。这正是 8 轮的生成机制之一。
//
// 三种"没收敛"的出路完全不同,判决必须把它们分开(混成一句「还剩 N 处重叠」等于
// 什么都没说)。

import (
	"strings"
	"testing"
)

func TestDestaggerStuckVerdict_Tiers(t *testing.T) {
	cases := []struct {
		name     string
		rep      destaggerRunReport
		maxRound int
		wantAny  []string // 必须出现的关键词
		wantNot  []string
	}{
		{
			name: "收敛了就不该有判决",
			rep:  destaggerRunReport{OverlapsAfter: 0, Rounds: 1},
			// 收敛 = 没有判决,空串。
			maxRound: 1,
		},
		{
			// 还在往前走,只是轮数用完 —— 出路就是继续跑,**不该**说"停手"。
			name:     "轮数用完但有进展",
			rep:      destaggerRunReport{OverlapsAfter: 2, Rounds: 3, Moved: make([]destaggerMove, 3)},
			maxRound: 3,
			wantAny:  []string{"轮上限用完", "--max-rounds"},
			wantNot:  []string{"停手"},
		},
		{
			// 还有重叠但一个都不敢搬:候选位全被占。**再跑一遍不会有别的结果** ——
			// 这一档必须说死,并给出换手段的三条路。
			name:     "一个都搬不动",
			rep:      destaggerRunReport{OverlapsAfter: 4, Rounds: 0},
			maxRound: 3,
			wantAny:  []string{"停手", "不会有别的结果", "sch connect", "group-move", "clusters"},
		},
		{
			name: "被逐条跳过",
			rep: destaggerRunReport{
				OverlapsAfter: 3, Rounds: 1, Moved: make([]destaggerMove, 1),
				Plan: destaggerPlan{Moves: make([]destaggerMove, 2), Skips: make([]destaggerSkip, 5)},
			},
			maxRound: 5,
			wantAny:  []string{"跳过", "skips", "不要盲目重跑"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := destaggerStuckVerdict(tc.rep, tc.maxRound)
			if tc.rep.OverlapsAfter == 0 {
				if got != "" {
					t.Fatalf("收敛了却给了判决:%s", got)
				}
				return
			}
			if got == "" {
				t.Fatal("没收敛却没有判决 —— 这正是要补的那个洞")
			}
			for _, w := range tc.wantAny {
				if !strings.Contains(got, w) {
					t.Fatalf("判决少了 %q:\n%s", w, got)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(got, w) {
					t.Fatalf("判决不该出现 %q(还有进展,出路是继续跑):\n%s", w, got)
				}
			}
		})
	}
}

// TestDestaggerStuckVerdict_ReportsRemainingCount:判决必须报出**还剩几处**。
// 「没收敛」三个字不可执行,数字才是下一步的依据。
func TestDestaggerStuckVerdict_ReportsRemainingCount(t *testing.T) {
	got := destaggerStuckVerdict(destaggerRunReport{OverlapsAfter: 7}, 1)
	if !strings.Contains(got, "7") {
		t.Fatalf("判决没报出剩余数:%s", got)
	}
}
