package protocol

import (
	"strings"
	"testing"
)

func TestPhase1ActionsHaveStableNames(t *testing.T) {
	actions := AllActions()
	if len(actions) == 0 {
		t.Fatal("expected actions")
	}

	seen := map[string]bool{}
	for _, action := range actions {
		if action.Name == "" {
			t.Fatalf("action has empty name: %#v", action)
		}
		if seen[action.Name] {
			t.Fatalf("duplicate action name: %s", action.Name)
		}
		seen[action.Name] = true
		if action.Phase < 1 {
			t.Fatalf("action %s has invalid phase %d", action.Name, action.Phase)
		}
	}

	for _, required := range []string{
		"system.health",
		"schematic.components.list",
		"schematic.component.place",
		"schematic.wire.create",
		"schematic.drc.check",
		"schematic.export.bom",
	} {
		if !seen[required] {
			t.Fatalf("missing required action: %s", required)
		}
	}
}

func TestEveryActionPublishesAnEffectiveTimeout(t *testing.T) {
	actions := AllActions()
	byName := make(map[string]ActionSpec, len(actions))
	for _, action := range actions {
		byName[action.Name] = action
		if action.TimeoutMs < DefaultActionTimeoutMs {
			t.Errorf("%s timeoutMs=%d, want at least catalog default %d", action.Name, action.TimeoutMs, DefaultActionTimeoutMs)
		}
	}
	for _, slow := range []string{
		"schematic.save",
		"schematic.rebind.footprint",
		"pcb.drc.check",
		"pcb.export.gerber",
	} {
		if got := byName[slow].TimeoutMs; got != 120_000 {
			t.Errorf("%s timeoutMs=%d, want 120000", slow, got)
		}
	}
}

func TestDryRunMetadataIsExplicitAndMatchesInputs(t *testing.T) {
	want := map[string]bool{
		"schematic.page.clear": true,
		"pcb.page.clear":       true,
		"pcb.beautify":         true,
	}
	for _, action := range AllActions() {
		hasDryRunInput := false
		for _, input := range action.Inputs {
			if strings.HasPrefix(strings.TrimSpace(input), "dryRun") {
				hasDryRunInput = true
				break
			}
		}
		if action.SupportsDryRun != hasDryRunInput {
			t.Errorf("%s SupportsDryRun=%v but dryRun input present=%v", action.Name, action.SupportsDryRun, hasDryRunInput)
		}
		if action.SupportsDryRun && !want[action.Name] {
			t.Errorf("unexpected dry-run action %s; review daemon write classification", action.Name)
		}
		if action.SupportsDryRun {
			delete(want, action.Name)
		}
	}
	for action := range want {
		t.Errorf("expected dry-run action %s is missing from catalog", action)
	}
}

func TestConnectPinActionDocumentsYUpContract(t *testing.T) {
	var description string
	for _, action := range AllActions() {
		if action.Name == "schematic.power.connect_pin" {
			description = action.Description
			break
		}
	}
	if description == "" {
		t.Fatal("schematic.power.connect_pin action missing")
	}
	for _, want := range []string{"y-UP", "up moves the endpoint to a larger y", "down to a smaller y"} {
		if !strings.Contains(description, want) {
			t.Errorf("connect_pin description missing %q: %s", want, description)
		}
	}
	if strings.Contains(description, "y-DOWN") {
		t.Errorf("connect_pin description still advertises y-DOWN: %s", description)
	}
}

func TestComponentsListDocumentsReadOnlyPreflightContract(t *testing.T) {
	var spec *ActionSpec
	for _, action := range AllActions() {
		if action.Name == "schematic.components.list" {
			copy := action
			spec = &copy
			break
		}
	}
	if spec == nil {
		t.Fatal("schematic.components.list action missing")
	}
	if spec.Mutates {
		t.Fatal("schematic.components.list must remain read-only (Mutates=false)")
	}
	text := strings.Join(append(append([]string{spec.Description}, spec.Inputs...), spec.Outputs...), " ")
	for _, want := range []string{
		"includeConnectivitySummary",
		"active page",
		"wires",
		"buses",
		"netflags",
		"netports",
		"netlabels",
		"pinsAvailable",
		"pinsError",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("components.list contract missing %q: %s", want, text)
		}
	}
}

func TestManufacturingSnapshotPublishesFailClosedReadOnlyContract(t *testing.T) {
	var spec *ActionSpec
	for _, action := range AllActions() {
		if action.Name == "pcb.manufacturing.snapshot" {
			copy := action
			spec = &copy
			break
		}
	}
	if spec == nil {
		t.Fatal("pcb.manufacturing.snapshot action missing")
	}
	if spec.Domain != DomainPcb || spec.Phase != 2 || !spec.NeedsWindow || spec.Mutates {
		t.Fatalf("unexpected manufacturing snapshot action metadata: %#v", *spec)
	}
	if spec.TimeoutMs != 120_000 {
		t.Fatalf("pcb.manufacturing.snapshot timeoutMs=%d, want 120000", spec.TimeoutMs)
	}
	text := strings.Join(append([]string{spec.Description}, spec.Outputs...), " ")
	for _, want := range []string{
		"fail-closed",
		"schemaVersion",
		"components",
		"component pads",
		"poured",
		"actual fill paths",
		"layers",
		"copperLayerCount",
		"drcRules",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("manufacturing snapshot contract missing %q: %s", want, text)
		}
	}
}

func TestDocumentCloseIsTypedAndConfirmationGated(t *testing.T) {
	for _, action := range AllActions() {
		if action.Name != "document.close" {
			continue
		}
		if action.Domain != DomainDocument || !action.NeedsWindow || action.Mutates || !action.ChangesContext || !action.NeedsConfirm {
			t.Fatalf("unexpected document.close metadata: %#v", action)
		}
		if !strings.Contains(strings.Join(action.Inputs, " "), "tabId") {
			t.Fatalf("document.close must require tabId: %#v", action)
		}
		return
	}
	t.Fatal("document.close action missing")
}

func TestForegroundContextActionsAreCataloguedSeparatelyFromMutations(t *testing.T) {
	want := map[string]bool{
		"document.open":       true,
		"document.close":      true,
		"schematic.page.open": true,
	}
	for _, action := range AllActions() {
		if want[action.Name] {
			if action.Mutates || !action.ChangesContext {
				t.Errorf("%s must be ChangesContext=true and Mutates=false: %#v", action.Name, action)
			}
			delete(want, action.Name)
		} else if action.ChangesContext {
			t.Errorf("unexpected context-changing action %s; review quarantine and MCP routing", action.Name)
		}
	}
	for missing := range want {
		t.Errorf("missing context-changing action %s", missing)
	}
}
