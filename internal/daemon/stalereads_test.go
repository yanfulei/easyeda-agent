package daemon

import (
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// staleReq builds a minimal request for the stale-read state machine.
func staleReq(action, windowID string, payload map[string]any) *protocol.Request {
	return &protocol.Request{
		Envelope: protocol.Envelope{WindowID: windowID},
		Action:   action,
		Payload:  payload,
	}
}

// runStale feeds one action (with the given outcome) through the guard and
// returns the response so callers can assert on StaleRisk.
func runStale(g *staleGuard, action, windowID string, ok bool, payload map[string]any) *protocol.Response {
	resp := &protocol.Response{OK: ok}
	g.observe(staleReq(action, windowID, payload), resp)
	return resp
}

func TestStaleGuard_MutationThenReadWarns(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.route.rip_up", "w1", true, nil)

	for _, read := range []string{"pcb.line.list", "pcb.via.list", "pcb.components.list", "pcb.drc.check", "pcb.pour.list"} {
		resp := runStale(g, read, "w1", true, nil)
		if resp.StaleRisk == "" {
			t.Errorf("%s after pcb.route.rip_up: want staleRisk advisory, got none", read)
			continue
		}
		if !strings.Contains(resp.StaleRisk, "pcb.route.rip_up") {
			t.Errorf("%s advisory should name the mutation, got %q", read, resp.StaleRisk)
		}
		if !strings.Contains(resp.StaleRisk, "doc reload") {
			t.Errorf("%s advisory should tell the fix (doc reload), got %q", read, resp.StaleRisk)
		}
	}
}

func TestStaleGuard_ReloadClears(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.via.create", "w1", true, nil)

	// `doc reload` is a CLI composite; its daemon-visible discriminator is the
	// typed document.close step (a doc switch/document.open must NOT clear).
	runStale(g, "document.close", "w1", true, map[string]any{"tabId": "tab-1"})

	if resp := runStale(g, "pcb.drc.check", "w1", true, nil); resp.StaleRisk != "" {
		t.Errorf("read after reload: want no staleRisk, got %q", resp.StaleRisk)
	}
}

func TestStaleGuard_PourRebuildClears(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.route.delete", "w1", true, nil)
	runStale(g, "pcb.pour.rebuild", "w1", true, nil)

	if resp := runStale(g, "pcb.line.list", "w1", true, nil); resp.StaleRisk != "" {
		t.Errorf("read after pour-rebuild: want no staleRisk, got %q", resp.StaleRisk)
	}
	// pour.rebuild is also exempt from marking: it must never re-arm the flag.
	if resp := runStale(g, "pcb.drc.check", "w1", true, nil); resp.StaleRisk != "" {
		t.Errorf("pour-rebuild must not itself mark stale, got %q", resp.StaleRisk)
	}
}

func TestStaleGuard_FailedMutationDoesNotMark(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.route.rip_up", "w1", false, nil)

	if resp := runStale(g, "pcb.line.list", "w1", true, nil); resp.StaleRisk != "" {
		t.Errorf("failed mutation must not mark stale, got %q", resp.StaleRisk)
	}
}

func TestStaleGuard_SaveDoesNotMarkOrClear(t *testing.T) {
	g := newStaleGuard()

	// pcb.save alone (e.g. explicit save, or a raw caller) never marks …
	runStale(g, "pcb.save", "w1", true, nil)
	if resp := runStale(g, "pcb.line.list", "w1", true, nil); resp.StaleRisk != "" {
		t.Errorf("pcb.save must not mark stale, got %q", resp.StaleRisk)
	}

	// … and does not clear an existing mark either (saving fixes nothing).
	runStale(g, "pcb.line.create", "w1", true, nil)
	runStale(g, "pcb.save", "w1", true, nil)
	if resp := runStale(g, "pcb.line.list", "w1", true, nil); resp.StaleRisk == "" {
		t.Error("pcb.save must not clear the stale mark")
	}
}

func TestStaleGuard_DocSwitchDoesNotClear(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.clear_routing", "w1", true, nil)

	// A foreground tab switch (document.open) does not reload engine state.
	runStale(g, "document.open", "w1", true, map[string]any{"uuid": "abc"})

	if resp := runStale(g, "pcb.drc.check", "w1", true, nil); resp.StaleRisk == "" {
		t.Error("document.open (doc switch) must NOT clear the stale mark — only a real reload does")
	}
}

func TestStaleGuard_SchematicMutationDoesNotMarkPcb(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "schematic.wire.create", "w1", true, nil)

	if resp := runStale(g, "pcb.line.list", "w1", true, nil); resp.StaleRisk != "" {
		t.Errorf("schematic mutation must not mark PCB stale, got %q", resp.StaleRisk)
	}
}

func TestStaleGuard_PerWindowIsolation(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.route.rip_up", "w1", true, nil)

	if resp := runStale(g, "pcb.line.list", "w2", true, nil); resp.StaleRisk != "" {
		t.Errorf("mutation on w1 must not flag reads on w2, got %q", resp.StaleRisk)
	}
	if resp := runStale(g, "pcb.line.list", "w1", true, nil); resp.StaleRisk == "" {
		t.Error("mutation on w1 should still flag reads on w1")
	}
}

func TestStaleGuard_MutatingActionsNotAnnotated(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.route.rip_up", "w1", true, nil)

	// A follow-up mutation is about to change the board anyway — no advisory.
	if resp := runStale(g, "pcb.line.create", "w1", true, nil); resp.StaleRisk != "" {
		t.Errorf("mutating action should not carry staleRisk, got %q", resp.StaleRisk)
	}
}

// TestStaleGuard_CatalogClassification pins the catalog-driven classification
// for the load-bearing copper mutations named by iron rule 5.
func TestStaleGuard_CatalogClassification(t *testing.T) {
	marks := []string{
		"pcb.route.rip_up", "pcb.route.delete", "pcb.route.via_hop",
		"pcb.clear_routing", "pcb.line.create", "pcb.via.create",
		"pcb.pour.create", "pcb.pour.delete", "pcb.import_autoroute",
	}
	for _, a := range marks {
		if !pcbStaleMarks(staleReq(a, "w1", nil)) {
			t.Errorf("pcbStaleMarks(%q) = false, want true", a)
		}
	}
	noMarks := []string{
		"pcb.save", "pcb.pour.rebuild", // exempt
		"pcb.line.list", "pcb.drc.check", // reads
		"schematic.wire.create", "document.open", // other domains
	}
	for _, a := range noMarks {
		if pcbStaleMarks(staleReq(a, "w1", nil)) {
			t.Errorf("pcbStaleMarks(%q) = true, want false", a)
		}
	}
	reads := []string{"pcb.line.list", "pcb.via.list", "pcb.pour.list", "pcb.components.list", "pcb.drc.check", "pcb.nets.list", "pcb.report", "pcb.board.info"}
	for _, a := range reads {
		if !pcbStaleRead(staleReq(a, "w1", nil)) {
			t.Errorf("pcbStaleRead(%q) = false, want true", a)
		}
	}
	if pcbStaleRead(staleReq("pcb.documents.list", "w1", nil)) {
		t.Error("pcb.documents.list is project metadata, not stale PCB engine state")
	}
	if pcbStaleRead(staleReq("schematic.components.list", "w1", nil)) {
		t.Error("schematic reads must not be classified as PCB stale reads")
	}
}

func TestStaleGuard_DocumentInventoryRemainsAvailableForReload(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.component.modify", "w1", true, nil)

	req := staleReq("pcb.documents.list", "w1", nil)
	if mutation := g.blockedBy(req); mutation != "" {
		t.Fatalf("pcb.documents.list blocked by %q; doc reload cannot resolve a named PCB", mutation)
	}
	resp := runStale(g, "pcb.documents.list", "w1", true, nil)
	if resp.StaleRisk != "" {
		t.Fatalf("project document inventory must not carry a PCB-engine stale warning: %q", resp.StaleRisk)
	}
}

// TestStaleGuard_DryRunNeverMarks pins issue #112b: `pcb clear --dry-run` only
// enumerates, so it must not arm the advisory for every later read. The catalog
// marks pcb.page.clear Mutates=true at action-name granularity; only the payload
// tells a preview from a real clear.
func TestStaleGuard_DryRunNeverMarks(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.page.clear", "w1", true, map[string]any{"dryRun": true})

	if resp := runStale(g, "pcb.line.list", "w1", true, nil); resp.StaleRisk != "" {
		t.Errorf("a dry-run clear changes nothing — later reads must be clean, got %q", resp.StaleRisk)
	}
	// The very same action WITHOUT the flag is a real clear and must arm it.
	runStale(g, "pcb.page.clear", "w1", true, map[string]any{"dryRun": false})
	resp := runStale(g, "pcb.line.list", "w1", true, nil)
	if resp.StaleRisk == "" {
		t.Fatal("a real pcb.page.clear must still arm the stale-read advisory")
	}
	if !strings.Contains(resp.StaleRisk, "pcb.page.clear") {
		t.Errorf("advisory should name the mutation, got %q", resp.StaleRisk)
	}
}

// TestStaleGuard_DryRunReadIsAnnotated: a preview reads the same engine state a
// list does, so when the window IS stale the dry-run's own counts are suspect and
// must carry the advisory (the 153→8 miscount that opened #112).
func TestStaleGuard_DryRunReadIsAnnotated(t *testing.T) {
	g := newStaleGuard()
	runStale(g, "pcb.route.rip_up", "w1", true, nil)

	resp := runStale(g, "pcb.page.clear", "w1", true, map[string]any{"dryRun": true})
	if resp.StaleRisk == "" {
		t.Error("a dry-run preview on a mutated-but-unreloaded window should be flagged stale")
	}
}

// TestIsDryRunRequest pins the payload convention: only a JSON `true` counts.
// Erring toward "this is a real write" keeps the safety nets armed.
func TestIsDryRunRequest(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"flag true", map[string]any{"dryRun": true}, true},
		{"flag false", map[string]any{"dryRun": false}, false},
		{"absent", map[string]any{"only": "routing"}, false},
		{"nil payload", nil, false},
		{"string true is not a bool", map[string]any{"dryRun": "true"}, false},
		{"snake_case is not the convention", map[string]any{"dry_run": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDryRunRequest(staleReq("pcb.page.clear", "w1", tc.payload)); got != tc.want {
				t.Errorf("isDryRunRequest(%v) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
	if isDryRunRequest(nil) {
		t.Error("nil request must not be treated as a dry run")
	}
	if isDryRunRequest(staleReq("schematic.component.place", "w1", map[string]any{"dryRun": true})) {
		t.Error("an undeclared dryRun field must never downgrade a placement to a preview")
	}
}
