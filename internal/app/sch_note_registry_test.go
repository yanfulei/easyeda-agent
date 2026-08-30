package app

import (
	"reflect"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/workflow"
)

func TestDropRegisteredNoteIDsCleansClaimsAndGroups(t *testing.T) {
	claims := map[string]*workflow.SchZoneClaim{
		"POWER": {NoteIDs: []string{"keep-claim", "drop-a"}},
	}
	groups := []*workflow.Group{
		{ID: "g1", NoteIDs: []string{"drop-a", "keep-group", "drop-b"}},
		{ID: "g2", NoteIDs: []string{"keep-2"}},
	}
	changed, removed := dropRegisteredNoteIDs(claims, groups, map[string]bool{"drop-a": true, "drop-b": true})
	if changed != 2 || removed != 3 {
		t.Fatalf("changed=%d removed=%d, want 2/3", changed, removed)
	}
	if !reflect.DeepEqual(claims["POWER"].NoteIDs, []string{"keep-claim"}) {
		t.Fatalf("claim note ids: %v", claims["POWER"].NoteIDs)
	}
	if !reflect.DeepEqual(groups[0].NoteIDs, []string{"keep-group"}) {
		t.Fatalf("group note ids: %v", groups[0].NoteIDs)
	}
	if !reflect.DeepEqual(groups[1].NoteIDs, []string{"keep-2"}) {
		t.Fatalf("unrelated group changed: %v", groups[1].NoteIDs)
	}
}

func TestDropRegisteredNoteIDsNoMatchIsNoop(t *testing.T) {
	claims := map[string]*workflow.SchZoneClaim{"POWER": {NoteIDs: []string{"n1"}}}
	groups := []*workflow.Group{{ID: "g1", NoteIDs: []string{"n2"}}}
	changed, removed := dropRegisteredNoteIDs(claims, groups, map[string]bool{"other": true})
	if changed != 0 || removed != 0 {
		t.Fatalf("changed=%d removed=%d, want 0/0", changed, removed)
	}
}
