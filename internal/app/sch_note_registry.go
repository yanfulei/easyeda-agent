package app

import (
	"fmt"
	"io"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

// dropRegisteredNoteIDs is the pure registry half of deleting schematic text.
// Notes can belong either to a hand-authored zone claim or to a persistent
// group. Keeping a deleted primitive id in either table makes later placement
// and note-outside-zone checks reason about a text that no longer exists.
func dropRegisteredNoteIDs(claims map[string]*workflow.SchZoneClaim, groups []*workflow.Group, drop map[string]bool) (changed, removed int) {
	filter := func(ids []string) ([]string, int) {
		kept := ids[:0]
		n := 0
		for _, id := range ids {
			if drop[id] {
				n++
				continue
			}
			kept = append(kept, id)
		}
		return kept, n
	}
	for _, claim := range claims {
		if claim == nil || len(claim.NoteIDs) == 0 {
			continue
		}
		ids, n := filter(claim.NoteIDs)
		if n > 0 {
			claim.NoteIDs = ids
			changed++
			removed += n
		}
	}
	for _, group := range groups {
		if group == nil || len(group.NoteIDs) == 0 {
			continue
		}
		ids, n := filter(group.NoteIDs)
		if n > 0 {
			group.NoteIDs = ids
			changed++
			removed += n
		}
	}
	return changed, removed
}

// cascadeSchNoteRegistrations removes only ids whose canvas deletion was
// verified. It is fail-soft because the drawing mutation has already landed;
// a persistence failure is reported with an executable audit command.
func cascadeSchNoteRegistrations(cfg *appConfig, window string, ids []string, stderr io.Writer) {
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			drop[id] = true
		}
	}
	if len(drop) == 0 {
		return
	}
	_, _, docUUID, _, st, groups, err := loadSchGroupsContext(cfg, window)
	if err != nil {
		fmt.Fprintf(stderr, "warn: note 登记级联清理不可用(%v)—— 用 `sch zones status` 审计 noteIds\n", err)
		return
	}
	claims := st.SchZonesForPage(docUUID)
	changed, removed := dropRegisteredNoteIDs(claims, groups, drop)
	if changed == 0 {
		return
	}
	st.SetSchZonesForPage(docUUID, claims)
	st.SetGroupsForPage(docUUID, groups)
	if err := savePcbStageState(st); err != nil {
		fmt.Fprintf(stderr, "warn: 已删说明的 noteIds 未能从登记表落盘(%v)—— 用 `sch zones status` 审计\n", err)
		return
	}
	fmt.Fprintf(stderr, "cascade ✓ 清理 %d 条已删说明的 noteIds(涉及 %d 个 zone/group)\n", removed, changed)
}
