package app

import (
	"fmt"
	"io"
)

// schMoveIDsUnverified 是 `sch group-move --ids` 的「移动做了、但电气自检没跑成」
// 出口。
//
// 为什么非零退出:--ids 只搬点名的图元,器件的桩线/旗**不会**跟着走,所以
// 「搬完没验」与「搬完验过」的风险天差地别 —— 前者可能已经静默断网。而它过去
// 只往 stderr 打一行 warning 就 return nil:退出码 0、stdout 上与自检通过
// 一模一样,脚本和人都会当成功。2026-08-26 实测正是这么连踩三件、留下
// 6 个 orphan-tree,直到几步之后 bridge-check 才翻出来。
//
// 口径与 `sch gate` 的 blocked 一致:**检查器没跑成 ≠ 板子没问题**,
// 两者的下一步也完全不同 —— 所以要报出来,并给出能执行的那一条。
func schMoveIDsUnverified(stdout io.Writer, what string, cause error) error {
	fmt.Fprintf(stdout, "⚠ 平移已执行,但**电气自检没跑成**(%s)—— 本次移动未经验证\n", what)
	fmt.Fprintln(stdout, "  --ids 只搬点名的图元:器件的桩线/旗不会跟随,断网不会有任何提示。")
	fmt.Fprintln(stdout, "  下一步:`easyeda sch bridge-check`(抓孤儿桩/孤儿树)+ `easyeda sch check`;")
	fmt.Fprintln(stdout, "  确认断了就 `easyeda sch autoconnect` 补回受影响的引脚,或改用 `sch group-move --group <id>`。")
	return fmt.Errorf("group-move --ids 已平移但电气自检未完成(%s:%v)—— "+
		"移动已落在画布上,请按上面的下一步自行验证;要带桩线+旗一起搬并自动重连,用 `sch group-move --group <id>`",
		what, cause)
}
