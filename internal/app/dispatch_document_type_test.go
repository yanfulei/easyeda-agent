package app

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

func TestActionDocumentType(t *testing.T) {
	tests := map[string]string{
		"pcb.components.list":       "pcb",
		"pcb.export.gerber":         "pcb",
		"schematic.components.list": "schematic",
		"document.current":          "",
	}
	for action, want := range tests {
		if got := actionDocumentType(action); got != want {
			t.Errorf("actionDocumentType(%q) = %q, want %q", action, got, want)
		}
	}
}

func TestValidateActionDocumentType(t *testing.T) {
	pcb := &protocol.ExpectedContext{ProjectUUID: "project-1", DocumentUUID: "pcb-1", DocumentType: "pcb"}
	schematic := &protocol.ExpectedContext{ProjectUUID: "project-1", DocumentUUID: "sch-1", DocumentType: "schematic"}
	if err := validateActionDocumentType("pcb.export.gerber", pcb); err != nil {
		t.Fatalf("PCB export rejected PCB binding: %v", err)
	}
	if err := validateActionDocumentType("schematic.check", schematic); err != nil {
		t.Fatalf("schematic action rejected schematic binding: %v", err)
	}
	if err := validateActionDocumentType("pcb.components.list", schematic); err == nil || !strings.Contains(err.Error(), "requires a pcb document") {
		t.Fatalf("PCB action with schematic binding error = %v", err)
	}
	if err := validateActionDocumentType("schematic.components.list", pcb); err == nil || !strings.Contains(err.Error(), "requires a schematic document") {
		t.Fatalf("schematic action with PCB binding error = %v", err)
	}
}

func TestExpectedContextBindingRequiresCompleteIdentity(t *testing.T) {
	target := openableDoc{UUID: "pcb-1", Type: "pcb", Name: "PCB1"}
	if _, err := expectedContextBinding(&actionContext{DocumentUUID: "pcb-1", DocumentType: "pcb"}, target); err == nil || !strings.Contains(err.Error(), "project UUID") {
		t.Fatalf("missing project UUID error = %v", err)
	}
	binding, err := expectedContextBinding(&actionContext{
		ProjectUUID: "project-1", DocumentUUID: "pcb-1", DocumentType: "pcb",
	}, target)
	if err != nil {
		t.Fatalf("complete binding rejected: %v", err)
	}
	if binding.ProjectUUID != "project-1" || binding.DocumentUUID != "pcb-1" || binding.DocumentType != "pcb" {
		t.Fatalf("unexpected binding: %+v", binding)
	}
}

func TestValidateProjectBinding(t *testing.T) {
	current := &actionContext{ProjectUUID: "project-1", ProjectName: "Probe"}
	if err := validateProjectBinding("project-1", current); err != nil {
		t.Fatalf("UUID selector rejected: %v", err)
	}
	if err := validateProjectBinding("Probe", current); err != nil {
		t.Fatalf("name selector rejected: %v", err)
	}
	if err := validateProjectBinding("Other", current); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong project selector error = %v", err)
	}
	if err := validateProjectBinding("Probe", &actionContext{}); err == nil || !strings.Contains(err.Error(), "context is missing") {
		t.Fatalf("missing live project identity error = %v", err)
	}
}

func TestEnsureActiveDocBindingRejectsProjectWindowMismatch(t *testing.T) {
	cfg, _, cleanup := newAutolayoutTestDaemon(t, func(_ int, call autolayoutTestCall) string {
		switch call.Action {
		case "document.current":
			return `{"ok":true,"result":{},"context":{"projectUuid":"actual-uuid","projectName":"Actual","documentUuid":"pcb-1","documentType":"pcb"}}`
		case "schematic.pages.list":
			return `{"ok":true,"result":{"pages":[]}}`
		case "pcb.documents.list":
			return `{"ok":true,"result":{"pcbs":[{"uuid":"pcb-1","name":"PCB1"}]}}`
		default:
			return `{"ok":true,"result":{}}`
		}
	})
	defer cleanup()
	cfg.project = "Requested"
	cfg.doc = "PCB1"
	if _, _, err := ensureActiveDocBinding(cfg, "w1"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong project/window binding error = %v", err)
	}
}
