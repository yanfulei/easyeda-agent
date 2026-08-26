package app

import (
	"math"
	"testing"
)

// 回归:place-constrained 把主控模组规划到**板框外**。
//
// 2026-08-26 esp32MiniRequire 端到端实测,板框 2400×1700、U2 是 748×1030 的
// ESP32-S3-WROOM-1(按名字判为 cpEdgeMust,贴边是有意的):
//
//	U2 → (1985, -1090) rot=0 edge=right      ← y 为负,整件在板框外
//
// dry-run 阶段即可复现。板子完全放得下它(2400×1700 装 748×1030),
// 所以这不是「装不下」,是贴边落点算错了。
//
// 判据故意写成**全体 move 都必须落在板框内**而不是只盯 U2:出框这件事对哪个
// 器件都是错的,钉死单个位号会让下一个出框的件重新溜过去。
func TestPlanConstrainedPlace_KeepsEveryMoveInsideBoard(t *testing.T) {
	board := cpRect{x0: 0, y0: 0, x1: 2400, y1: 1700}
	// **关键前提**:`pcb import-changes` 刚导进来的器件散布在板框**外**
	// (实测全在负 y 区:U2 anchor 是 (4799,-1088))。事故里 U2 被规划到
	// (1985,-1090) —— 贴边轴 x 挪到了 1985,而**沿边轴 y 几乎没动**
	// (-1088 → -1090):placeEdgePart 只算贴边那一个轴的 shift,沿边轴没有
	// 任何边界夹持,于是器件原本在板外就一直留在板外。
	comps := []cpComp{
		mkCP("U2", "esp32-s3-wroom-1", 1, 4799, -1088, 748, 1030, 41),
		mkCP("J1", "conn.usb_c_16p", 1, 3387, -870, 474, 351, 16),
		mkCP("J2", "conn.screw_terminal_2p", 1, 770, -674, 434, 316, 2),
		mkCP("U3", "ch340c", 1, 2877, -409, 418, 299, 16),
		mkCP("U1", "ldo.ams1117_3v3", 1, 1454, -331, 362, 268, 4),
		mkCP("C1", "cap.100nf", 1, 1710, -331, 75, 45, 2),
		mkCP("R1", "res.10k", 1, 3955, -395, 80, 45, 2),
		mkCP("LED1", "led.red_0805", 1, 5560, -242, 176, 91, 2),
	}
	holes := []cpHole{
		{x: 155, y: 155, r: 118}, {x: 2245, y: 155, r: 118},
		{x: 155, y: 1545, r: 118}, {x: 2245, y: 1545, r: 118},
	}
	opt := defaultCpOptions()
	opt.board = &board

	moves, _ := planConstrainedPlace(comps, holes, opt)
	if len(moves) == 0 {
		t.Fatal("一个 move 都没规划出来 —— 用例失去意义")
	}

	byDes := map[string]cpComp{}
	for _, c := range comps {
		byDes[c.designator] = c
	}
	for _, m := range moves {
		c, ok := byDes[m.Designator]
		if !ok {
			t.Fatalf("规划了一个不存在的器件 %s", m.Designator)
		}
		// move 给的是 anchor;bbox 随 anchor 平移。planner 可能连带转 90°
		// (边缘连接器要把开口朝外),那时长宽互换 —— 不换就会把一次**正确**的
		// 贴边误判成出框。
		w, h := c.maxX-c.minX, c.maxY-c.minY
		if m.SetRot && math.Abs(math.Mod(m.NewRot-c.rotation, 180)) > 1 {
			w, h = h, w
		}
		x0, y0 := m.NewX-w/2, m.NewY-h/2
		x1, y1 := m.NewX+w/2, m.NewY+h/2
		if x0 < board.x0 || y0 < board.y0 || x1 > board.x1 || y1 > board.y1 {
			t.Errorf("%s 被规划到板框外:bbox (%.0f,%.0f)-(%.0f,%.0f) 超出板框 (%.0f,%.0f)-(%.0f,%.0f)",
				m.Designator, x0, y0, x1, y1, board.x0, board.y0, board.x1, board.y1)
		}
	}
}

// 板子真的装不下时,不能靠「悄悄放到框外」来假装成功 —— 要么不给这个 move,
// 要么在 diag 里说清楚。无论如何都不许产出一个落在框外的坐标。
func TestPlanConstrainedPlace_OversizedPartNeverLandsOutside(t *testing.T) {
	board := cpRect{x0: 0, y0: 0, x1: 600, y1: 400}
	comps := []cpComp{
		mkCP("U2", "esp32-s3-wroom-1", 1, 300, 200, 748, 1030, 41), // 比板子还大
		mkCP("C1", "cap.100nf", 1, 100, 100, 75, 45, 2),
	}
	opt := defaultCpOptions()
	opt.board = &board

	moves, diags := planConstrainedPlace(comps, holes(), opt)
	for _, m := range moves {
		if m.Designator != "U2" {
			continue
		}
		x0, y0 := m.NewX-374, m.NewY-515
		x1, y1 := m.NewX+374, m.NewY+515
		// 比板子还大的件当然「框不住」,但**锚点**不该跑到板外去:
		// 那会让后续所有判据(zone/lint/DRC)都从一个荒谬的坐标开始。
		if m.NewX < board.x0 || m.NewX > board.x1 || m.NewY < board.y0 || m.NewY > board.y1 {
			t.Errorf("超尺寸件的锚点被放到板框外:(%.0f,%.0f) bbox (%.0f,%.0f)-(%.0f,%.0f);diags=%v",
				m.NewX, m.NewY, x0, y0, x1, y1, diags)
		}
	}
}

func holes() []cpHole { return nil }
