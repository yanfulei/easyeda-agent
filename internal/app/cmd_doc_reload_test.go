package app

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRequireConfirmedSave(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  *actionResult
		ok   bool
	}{
		{name: "saved true", res: &actionResult{Result: map[string]any{"saved": true}}, ok: true},
		{name: "saved false", res: &actionResult{Result: map[string]any{"saved": false}}},
		{name: "missing saved", res: &actionResult{Result: map[string]any{}}},
		{name: "wrong type", res: &actionResult{Result: map[string]any{"saved": "true"}}},
		{name: "nil response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireConfirmedSave("pcb.save", tc.res)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("reload must not close a document without exact saved:true")
			}
		})
	}
}

func TestRequireConfirmedClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  *actionResult
		ok   bool
	}{
		{name: "closed true", res: &actionResult{Result: map[string]any{"closed": true}}, ok: true},
		{name: "closed false", res: &actionResult{Result: map[string]any{"closed": false}}},
		{name: "missing closed", res: &actionResult{Result: map[string]any{}}},
		{name: "wrong type", res: &actionResult{Result: map[string]any{"closed": "true"}}},
		{name: "nil response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireConfirmedClose(tc.res)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("reload must not reopen without exact closed:true")
			}
		})
	}
}

func TestReloadDocumentWaitsForReopenedInventoryToSettle(t *testing.T) {
	context := &actionContext{DocumentUUID: "pcb-1", DocumentType: "pcb", TabID: "tab-1"}
	var sequence []string
	rt := reloadDocumentRuntime{
		request: func(_ *appConfig, action, _ string, _ any) (*actionResult, error) {
			sequence = append(sequence, action)
			res := &actionResult{OK: true, Context: context, Result: map[string]any{}}
			switch action {
			case "pcb.save":
				res.Result["saved"] = true
			case "document.close":
				res.Result["closed"] = true
			}
			return res, nil
		},
		settle: func(_ *appConfig, window, docType string) bool {
			sequence = append(sequence, "settle:"+window+":"+docType)
			return true
		},
		sleep: func(time.Duration) {},
		now:   time.Now,
	}

	docType, err := reloadDocumentByUUIDWithRuntime(&appConfig{}, "w1", "pcb-1", rt)
	if err != nil || docType != "pcb" {
		t.Fatalf("reload = type %q err %v", docType, err)
	}
	want := []string{"document.current", "pcb.save", "document.close", "document.open", "document.current", "settle:w1:pcb"}
	if !reflect.DeepEqual(sequence, want) {
		t.Fatalf("sequence = %v, want %v", sequence, want)
	}

	rt.settle = func(*appConfig, string, string) bool { return false }
	_, err = reloadDocumentByUUIDWithRuntime(&appConfig{}, "w1", "pcb-1", rt)
	if err == nil || !strings.Contains(err.Error(), "inventory did not settle") {
		t.Fatalf("unsettled reload must fail explicitly, got %v", err)
	}
}

func TestDiscoverDocsUsesLiveEasyEDAProtocolShape(t *testing.T) {
	cfg, daemon, cleanup := newAutolayoutTestDaemon(t, func(_ int, call autolayoutTestCall) string {
		switch call.Action {
		case "document.current":
			return `{"ok":true,"result":{"uuid":"pcb-1","tabId":"tab-pcb-1","documentType":"pcb","documentTypeCode":3,"parentProjectUuid":"project-1"},"context":{"projectUuid":"project-1","projectName":"Probe","documentUuid":"pcb-1","documentType":"pcb","tabId":"tab-pcb-1"}}`
		case "schematic.pages.list":
			return `{"ok":true,"result":{"schematics":[{"uuid":"sch-1","name":"probe","parentProjectUuid":"project-1","page":[{"uuid":"page-1","name":"POWER_PROBE","parentSchematicUuid":"sch-1"}]}],"pages":[{"uuid":"page-1","name":"POWER_PROBE","parentSchematicUuid":"sch-1"}]}}`
		case "pcb.documents.list":
			return `{"ok":true,"result":{"pcbs":[{"uuid":"pcb-1","name":"PCB1","parentProjectUuid":"project-1"}],"count":1}}`
		default:
			return `{"ok":false,"error":{"code":"UNEXPECTED_ACTION","message":"unexpected action"}}`
		}
	})
	defer cleanup()
	cfg.project = "Probe"

	docs, active, window, err := discoverDocs(cfg, "w1")
	if err != nil {
		t.Fatalf("discoverDocs: %v", err)
	}
	if active != "pcb-1" || window != "w1" {
		t.Fatalf("active=%q window=%q, want pcb-1/w1", active, window)
	}
	if len(docs) != 2 {
		t.Fatalf("documents=%+v, want schematic + PCB", docs)
	}
	pcb, err := resolveDoc(docs, "PCB1")
	if err != nil {
		t.Fatalf("resolve active PCB by friendly name: %v", err)
	}
	if pcb.UUID != "pcb-1" || pcb.Type != "pcb" || !pcb.Active {
		t.Fatalf("resolved PCB=%+v", pcb)
	}
	calls := daemon.snapshot()
	if len(calls) != 3 || calls[0].Action != "document.current" ||
		calls[1].Action != "schematic.pages.list" || calls[2].Action != "pcb.documents.list" {
		t.Fatalf("unexpected discovery sequence: %+v", calls)
	}
}

func TestDiscoverDocsDoesNotHidePcbInventoryFailure(t *testing.T) {
	cfg, _, cleanup := newAutolayoutTestDaemon(t, func(_ int, call autolayoutTestCall) string {
		switch call.Action {
		case "document.current":
			return `{"ok":true,"result":{},"context":{"projectUuid":"project-1","projectName":"Probe","documentUuid":"pcb-1","documentType":"pcb","tabId":"tab-pcb-1"}}`
		case "schematic.pages.list":
			return `{"ok":true,"result":{"pages":[{"uuid":"page-1","name":"POWER_PROBE","parentSchematicUuid":"sch-1"}]}}`
		case "pcb.documents.list":
			return `{"ok":false,"error":{"code":"STALE_READ","message":"pcb.documents.list blocked"}}`
		default:
			return `{"ok":true,"result":{}}`
		}
	})
	defer cleanup()
	cfg.project = "Probe"

	_, _, _, err := discoverDocs(cfg, "w1")
	if err == nil || !strings.Contains(err.Error(), "list PCB documents") || !strings.Contains(err.Error(), "pcb.documents.list blocked") {
		t.Fatalf("PCB inventory failure must stay actionable, got %v", err)
	}
}
