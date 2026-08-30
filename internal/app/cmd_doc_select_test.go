package app

import (
	"testing"
	"time"
)

func win(id, proj string) healthWindow {
	var w healthWindow
	w.WindowID = id
	w.Context.ProjectName = proj
	return w
}

func exactDocWin(id, proj, doc, docType, tab string, connectedAt time.Time) healthWindow {
	w := win(id, proj)
	w.Context.ProjectUUID = "project-uuid"
	w.Context.DocumentUUID = doc
	w.Context.DocumentType = docType
	w.Context.TabID = tab
	w.ConnectedAt = connectedAt
	return w
}

func TestSelectWindow(t *testing.T) {
	a := win("w-esp", "立创·实战派ESP32-S3开发板")
	b := win("w-moto", "motobox2026")

	// explicit --window always wins, even with many windows
	if id, err := selectWindow([]healthWindow{a, b}, "", "w-x"); err != nil || id != "w-x" {
		t.Fatalf("explicit window: %q %v", id, err)
	}
	// --project unique match (CJK name)
	if id, err := selectWindow([]healthWindow{a, b}, "立创·实战派ESP32-S3开发板", ""); err != nil || id != "w-esp" {
		t.Fatalf("project match: %q %v", id, err)
	}
	// sole window, no project
	if id, err := selectWindow([]healthWindow{b}, "", ""); err != nil || id != "w-moto" {
		t.Fatalf("sole window: %q %v", id, err)
	}
	// 2+ windows, no project → error
	if _, err := selectWindow([]healthWindow{a, b}, "", ""); err == nil {
		t.Fatal("expected multi-window error")
	}
	// project not found → error
	if _, err := selectWindow([]healthWindow{a, b}, "ghost", ""); err == nil {
		t.Fatal("expected no-match error")
	}
	// project maps to 2 windows → error
	if _, err := selectWindow([]healthWindow{win("w1", "dup"), win("w2", "dup")}, "dup", ""); err == nil {
		t.Fatal("expected ambiguous-project error")
	}
	// Duplicate activations of the exact same document tab collapse to newest.
	older := exactDocWin("w-old", "dup", "doc-1", "pcb", "tab-1", time.Unix(1, 0))
	newer := exactDocWin("w-new", "dup", "doc-1", "pcb", "tab-1", time.Unix(2, 0))
	if id, err := selectWindow([]healthWindow{older, newer}, "dup", ""); err != nil || id != "w-new" {
		t.Fatalf("duplicate project document: %q %v", id, err)
	}
	if id, err := selectWindow([]healthWindow{older, newer}, "", ""); err != nil || id != "w-new" {
		t.Fatalf("duplicate sole document: %q %v", id, err)
	}
	// Two real documents in one project remain ambiguous.
	schematic := exactDocWin("w-sch", "dup", "doc-sch", "schematic", "tab-sch", time.Unix(3, 0))
	if _, err := selectWindow([]healthWindow{newer, schematic}, "dup", ""); err == nil {
		t.Fatal("expected distinct project documents to remain ambiguous")
	}
	// no windows → error
	if _, err := selectWindow(nil, "", ""); err == nil {
		t.Fatal("expected no-connector error")
	}
}
