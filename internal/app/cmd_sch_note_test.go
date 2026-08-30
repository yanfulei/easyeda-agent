package app

import "testing"

func TestFindNewSchNoteOnlyAdoptsPostSnapshotExactMatch(t *testing.T) {
	before := map[string]bool{"old": true}
	texts := []zoneMoveText{
		{ID: "old", Content: "same", X: 100, Y: 200},
		{ID: "wrong-content", Content: "other", X: 100, Y: 200},
		{ID: "wrong-place", Content: "same", X: 120, Y: 200},
		{ID: "new", Content: "same", X: 100.25, Y: 199.75},
	}
	id, count := findNewSchNote(texts, before, "same", 100, 200)
	if id != "new" || count != 1 {
		t.Fatalf("got id=%q count=%d, want new/1", id, count)
	}
}

func TestFindNewSchNoteReportsAmbiguousMatches(t *testing.T) {
	texts := []zoneMoveText{
		{ID: "a", Content: "same", X: 100, Y: 200},
		{ID: "b", Content: "same", X: 100, Y: 200},
	}
	_, count := findNewSchNote(texts, nil, "same", 100, 200)
	if count != 2 {
		t.Fatalf("count=%d, want 2", count)
	}
}
