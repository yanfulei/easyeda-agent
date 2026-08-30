package app

// cmd_sch_note.go — `sch note`: put a CIRCUIT-DESCRIPTION text note on the
// schematic sheet (电路说明).
//
// Why this exists: functional partitioning is only half of the "先看区、再看线"
// convention — a zone frame names a module, but a reviewer still needs the one
// or two lines that say what the block does and what its key parameters are
// (「LDO 5V→3V3 1A」「BOOT: GPIO0 拉低进烧录」). User feedback: agent-produced
// schematics shipped with zones but no descriptions, so the skill now treats a
// short note per module as part of the layout default — and this command is the
// typed path that makes that default executable.
//
// Implementation note: notes use the connector's typed schematic.text.create
// action, followed by schematic.text.list verification before any retry. Notes
// are plain text primitives: enumerate them with `sch text-list`, remove with
// `sch prim-delete --ids`.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// schNoteDefaultFontSize keeps notes visually subordinate to zone labels
// (14) and partition titles (22): a note is an annotation, not a heading.
const schNoteDefaultFontSize = 10.0

// schNoteDefaultColor is a mid gray — readable, but clearly annotation-tier
// against the magenta zone frames and the black circuit.
const schNoteDefaultColor = "#5A5A5A"

// EasyEDA Pro's beta schematic text API rejects closely spaced writes by
// resolving create() to undefined. Live 3.2.x runs only recovered after roughly
// 15 seconds; the generic 400 ms settle window is therefore too short here.
// This delay is used only after a fresh read proves that the failed write did
// not land, so waiting and resending cannot duplicate a note.
const schematicTextRetryDelay = 15 * time.Second

func readSchNoteTexts(cfg *appConfig, window, docUUID, phase string) ([]zoneMoveText, error) {
	res, err := requestAutolayoutAction(cfg, "schematic.text.list", window, nil, docUUID, phase)
	if err != nil {
		return nil, err
	}
	return parseZoneMoveTexts(res.Result), nil
}

// findNewSchNote matches only ids absent from the pre-write snapshot. A text
// already at the same coordinate can therefore never be adopted after a lost
// response, and an ambiguous multi-match fails closed.
func findNewSchNote(texts []zoneMoveText, before map[string]bool, content string, x, y float64) (string, int) {
	var id string
	count := 0
	for _, t := range texts {
		if before[t.ID] || t.Content != content || absF(t.X-x) > zoneAnchorEps || absF(t.Y-y) > zoneAnchorEps {
			continue
		}
		id = t.ID
		count++
	}
	return id, count
}

// createSchNoteTyped implements verify-before-retry for a mutating typed
// action. EasyEDA can return no primitive or lose a response after landing; a
// fresh text-list decides which case occurred before a second send is allowed.
func createSchNoteTyped(cfg *appConfig, window, docUUID, content, color string, x, y, fontSize float64) (string, error) {
	beforeTexts, err := readSchNoteTexts(cfg, window, docUUID, "snapshot notes before create")
	if err != nil {
		return "", fmt.Errorf("read notes before create: %w", err)
	}
	before := make(map[string]bool, len(beforeTexts))
	for _, t := range beforeTexts {
		before[t.ID] = true
	}
	payload := map[string]any{
		"x": x, "y": y, "content": content, "rotation": 0,
		"color": color, "fontSize": fontSize,
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		phase := "create schematic note"
		if attempt > 0 {
			phase += " (retry)"
		}
		res, werr := requestAutolayoutActionTimed(cfg, "schematic.text.create", window, payload, 30*time.Second, docUUID, phase)
		if werr == nil {
			id := asString(res.Result["primitiveId"])
			if id == "" {
				return "", fmt.Errorf("schematic.text.create returned no primitiveId: %v", res.Result)
			}
			return id, nil
		}
		lastErr = werr

		after, rerr := readSchNoteTexts(cfg, window, docUUID, "verify schematic note after failed write")
		if rerr != nil {
			return "", fmt.Errorf("%v; landed-check also failed (%v) — refusing to resend", werr, rerr)
		}
		id, matches := findNewSchNote(after, before, content, x, y)
		switch matches {
		case 1:
			reportWriteVerified(cfg, window, writeVerdict{
				action: "schematic.text.create", source: "sch note",
				returnedOK: false, landed: 1,
			})
			return id, nil
		case 0:
			if attempt == 0 {
				time.Sleep(schematicTextRetryDelay)
				continue
			}
		default:
			return "", fmt.Errorf("%v; landed-check found %d new matching texts — ambiguous, refusing to resend", werr, matches)
		}
	}
	return "", lastErr
}

// newSchNoteCmd builds `sch note`.
func newSchNoteCmd(cfg *appConfig, window *string, stdout, stderr io.Writer) *cobra.Command {
	var text, color, zoneRef string
	var x, y, fontSize float64
	var asJSON bool
	c := &cobra.Command{
		Use:   "note",
		Short: "Place a circuit-description text note (电路说明) on the schematic sheet",
		Long: `Place a circuit-description text note (电路说明) on the schematic sheet.

Functional partitioning is only half of the layout convention — a zone frame
names a module, but a reviewer still needs the one or two lines that say what
the block does and what its key parameters are. The skill's schematic layout
default is: one short note per module, parked just below/beside its zone frame.

  - **省略 --x/--y = 自动落点(推荐)**:说明文字和器件、marker、已有文字、图签
    keep-out 是**同级的布局对象**,一起进同一张碰撞表求解。给了 --zone 时,说明
    的家是该区分区框底部的**说明带**,并且**贴着框底**落:
    note.y = 分区框.minY + 16 —— 文字锚点是块的左下角(块向上生长),所以离框底
    的距离与行数/字号无关,同一页所有说明底边齐平。
    带内放不下时:先按框宽折行,框装不下就把框**横向扩边**(窄框扩到最小可读
    宽度),带底被邻区桩线占住就把框底**下探**到占用之下、说明仍贴着新的框底 ——
    **框为说明扩边,而不是把说明踢出框**,更不会为了贴底压到器件上(真装不下
    会如实报告并说清是哪一维不够)。扩过边要重跑
    sch zone-draw --mode partition 让画布上的框跟上(命令会在 stderr 提示)。
  - 区里实在装不下(可扩边界被纸边/图签/邻框顶死)才退到区外走廊/整页扫描,
    并明确警告;整页都放不下就报错**拒绝画**,不把说明糊在电路上。
  - 显式给 --x/--y 时坐标一字不改,但仍会回读碰撞;压到东西会明确警告(不静默)。
  - Multi-line: a literal \n in --text becomes a real line break.
  - Coordinates are schematic units, y-UP (larger y = higher on the sheet).
  - Notes are plain text primitives: enumerate with sch text-list, remove with
    sch prim-delete --ids. The schematic is saved after a successful create.`,
		Example: `  easyeda sch note --zone POWER --text "LDO: 5V→3V3 1A\n输入 22µF / 输出 22µF"   # 自动落点
  easyeda sch note --text "BOOT: GPIO0 拉低进烧录" --x 640 --y 210 --font-size 9        # 显式坐标`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("--text is required (the note content)")
			}
			if fontSize <= 0 {
				fontSize = schNoteDefaultFontSize
			}
			// A literal backslash-n typed in a shell argument means "line break".
			content := strings.ReplaceAll(text, `\n`, "\n")

			pinnedCfg, win, docUUID, err := pinZonePage(cfg, *window)
			if err != nil {
				return err
			}
			// 不给坐标 = 让代码算:说明与器件/marker/已有文字/图签 keep-out 同级
			// 参与碰撞求解(用户纠偏 2026-08-13)。给了坐标就一字不改地照放,
			// 但仍然回读一次碰撞并在压到东西时明确警告 —— 不静默画上去。
			auto := !cmd.Flags().Changed("x") && !cmd.Flags().Changed("y")
			warns, zoneMatched, aerr := placeSchNote(pinnedCfg, win, docUUID, zoneRef, &content, fontSize, auto, &x, &y)
			if aerr != nil {
				return aerr
			}
			for _, wmsg := range warns {
				fmt.Fprintf(stderr, "warning: %s\n", wmsg)
			}
			// The typed create path snapshots text ids and performs a light read
			// before retrying, so a lost response cannot duplicate the note.
			tid, err := createSchNoteTyped(pinnedCfg, win, docUUID, content, color, x, y, fontSize)
			if err != nil {
				return err
			}
			if err := saveZoneDocument(pinnedCfg, win, docUUID, "save schematic note"); err != nil {
				return err
			}
			// --zone:把说明登记为功能区的内置对象(Zone = 外框+标题+说明+组+散件,
			// 用户定义的对象模型)。登记后 zone move 无条件带走(不再依赖"锚点恰好
			// 在框内"的几何猜)。注意:登记的说明**不再**反哺分区框的内容 bbox
			// (根因 C 的自增长反馈环已断)——它的家是分区框内的说明带。
			emit := func(registered bool) error {
				if asJSON {
					out := map[string]any{"textId": tid, "x": x, "y": y, "page": docUUID, "saved": true}
					if zoneRef != "" {
						out["zone"] = zoneRef
						out["registered"] = registered
						// zoneMatched = 该区是否在本页分区计划里命中(命中才有
						// 说明带可落;false = 已按整页避让兜底,见 stderr warning)。
						out["zoneMatched"] = zoneMatched
					}
					enc := json.NewEncoder(stdout)
					enc.SetIndent("", "  ")
					return enc.Encode(out)
				}
				if zoneRef != "" {
					fmt.Fprintf(stdout, "note created (primitiveId %s) at (%g, %g), registered to zone %q (zoneMatched=%v); schematic saved\n", tid, x, y, zoneRef, zoneMatched)
					return nil
				}
				fmt.Fprintf(stdout, "note created (primitiveId %s) at (%g, %g) on page %s; schematic saved\n", tid, x, y, docUUID)
				return nil
			}
			if zoneRef != "" {
				if rerr := registerSchZoneNote(pinnedCfg, win, docUUID, zoneRef, tid); rerr != nil {
					return fmt.Errorf("note %s 已创建并保存,但登记到区 %q 失败:%w(可重跑 `sch note` 前先 prim-delete,或忽略登记)", tid, zoneRef, rerr)
				}
				return emit(true)
			}
			return emit(false)
		},
	}
	c.Flags().StringVar(&text, "text", "", "note content; a literal \\n becomes a line break (required)")
	c.Flags().Float64Var(&x, "x", 0, "text anchor x (schematic units) — 省略 --x/--y 即自动落点(推荐):说明与器件/marker/已有文字/图签 keep-out 同级参与碰撞求解,自己找不压任何东西的位置")
	c.Flags().Float64Var(&y, "y", 0, "text anchor y (schematic units, y-UP) — 省略即自动落点")
	c.Flags().Float64Var(&fontSize, "font-size", schNoteDefaultFontSize, "font size")
	c.Flags().StringVar(&color, "color", schNoteDefaultColor, "text color")
	c.Flags().StringVar(&zoneRef, "zone", "", "把说明登记到一个布局对象(模块认领/块组/子组统一命名空间,全名/末段短名/组 id/唯一前缀均可,`sch zones status` 看全表)—— 自动落点**贴着该区分区框底边**落进框内说明带(离框底恒 16,与行数/字号无关),`sch zone move` 无条件带走它")
	c.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出结果(textId/x/y/zoneMatched 等)")
	_ = c.MarkFlagRequired("text")
	return c
}

// registerSchZoneNote 把新建说明的 primitiveId 记到它所属的布局对象名下(幂等)。
//
// **解析统一,写回分叉**:--zone 走统一注册表解析(resolveLayoutObject,模块认领 /
// 块组 / 子组同一张表,子组末段与组 id 都是别名),但写必须落到数据真正的家 ——
// 命中虚拟组就写 Group.NoteIDs,命中认领就写 claim.NoteIDs(zoneClaim 对认领
// 直通存储指针,对组是投影 —— 往投影上写会随返回值一起蒸发,所以组走 o.Group)。
func registerSchZoneNote(cfg *appConfig, window, docUUID, zoneRef, textID string) error {
	project, err := resolveStageProject(cfg, window)
	if err != nil {
		return err
	}
	st, err := loadPcbStageState(project)
	if err != nil {
		return err
	}
	claims := st.SchZonesForPage(docUUID)
	groups := st.GroupsForPage(docUUID)
	obj, err := resolveLayoutObject(buildLayoutObjectTable(claims, groups), zoneRef)
	if err != nil {
		return err
	}
	if obj.Group != nil {
		for _, id := range obj.Group.NoteIDs {
			if id == textID {
				return nil // 幂等
			}
		}
		obj.Group.NoteIDs = append(obj.Group.NoteIDs, textID)
		return saveSchGroups(st, docUUID, groups)
	}
	for _, id := range obj.Claim.NoteIDs {
		if id == textID {
			return nil
		}
	}
	obj.Claim.NoteIDs = append(obj.Claim.NoteIDs, textID)
	st.SetSchZonesForPage(docUUID, claims)
	return savePcbStageState(st)
}
