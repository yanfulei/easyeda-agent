package app

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func releaseTestCSV(t *testing.T, encoding releaseCSVEncoding, comma rune, records [][]string) []byte {
	t.Helper()
	data, err := encodeReleaseCSVTable(&releaseCSVTable{Encoding: encoding, Comma: comma}, records)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseReleaseManufacturingSnapshotSeparatesPopulationsAndHoleClasses(t *testing.T) {
	snapshot := releaseTestSnapshot()
	components := snapshot["components"].([]any)
	tht := map[string]any{
		"primitiveId": "p-j1", "designator": "J1", "layer": float64(1), "addIntoBom": true,
		"manufacturerId": "KF301", "supplierId": "C474881", "footprint": map[string]any{"name": "CONN-TH"},
		"x": float64(100), "y": float64(200), "rotation": float64(0),
		"pads": []any{map[string]any{
			"primitiveId": "j1-pad", "layer": float64(1), "hole": map[string]any{"diameter": float64(32)}, "metallization": true,
		}},
	}
	dnp := map[string]any{
		"primitiveId": "p-r2", "designator": "R2", "layer": float64(1), "addIntoBom": false,
		"manufacturerId": "MPN-R2", "supplierId": "C999", "footprint": map[string]any{"name": "R0402"},
		"x": float64(300), "y": float64(400), "rotation": float64(0),
		"pads": []any{map[string]any{"primitiveId": "r2-pad", "layer": float64(1), "hole": nil, "metallization": false}},
	}
	components = append(components, tht, dnp)
	snapshot["components"] = components
	snapshot["pads"] = append(snapshot["pads"].([]any),
		tht["pads"].([]any)[0],
		map[string]any{"primitiveId": "mount", "layer": float64(12), "hole": map[string]any{"diameter": float64(120)}, "metallization": false},
	)

	parsed, err := parseReleaseManufacturingSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.BOMReferences, []string{"C1", "J1", "R1"}) {
		t.Fatalf("BOM population = %v", parsed.BOMReferences)
	}
	if !reflect.DeepEqual(parsed.CPLReferences, []string{"C1", "R1"}) {
		t.Fatalf("CPL population = %v", parsed.CPLReferences)
	}
	if !parsed.NeedsPTH || !parsed.NeedsNPTH || !parsed.TopSMT || parsed.BottomSMT {
		t.Fatalf("unexpected fabrication/assembly facts: %+v", parsed)
	}
	before := parsed.SHA256
	snapshot["lines"] = []any{map[string]any{"primitiveId": "track-1", "endX": float64(42)}}
	after, err := parseReleaseManufacturingSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if before == after.SHA256 {
		t.Fatal("complete snapshot hash must change when copper geometry changes")
	}
}

func TestAuditReleaseBOMHandlesUTF16GroupedRowsAndValidatesFields(t *testing.T) {
	components := []releaseComponent{
		{Designator: "C1", AddIntoBOM: true, Footprint: "C0402", Manufacturer: "CL05B104", Supplier: "C1525"},
		{Designator: "C2", AddIntoBOM: true, Footprint: "C0402", Manufacturer: "CL05B104", Supplier: "C1525"},
		{Designator: "R9", AddIntoBOM: false, Footprint: "R0402", Manufacturer: "DNP", Supplier: "C9"},
	}
	data := releaseTestCSV(t, releaseCSVUTF16LE, '\t', [][]string{
		{"Quantity", "Designator", "Footprint", "Manufacturer Part", "Supplier Part"},
		{"2", "C1,C2", "C0402", "CL05B104", "C1525"},
	})
	inspection, err := auditReleaseBOM(data, components)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Matched || inspection.Rows != 1 || !reflect.DeepEqual(inspection.References, []string{"C1", "C2"}) {
		t.Fatalf("unexpected inspection: %+v", inspection)
	}

	bad := releaseTestCSV(t, releaseCSVUTF16LE, '\t', [][]string{
		{"Quantity", "Designator", "Footprint", "Manufacturer Part", "Supplier Part"},
		{"2", "C1,C2", "C0603", "CL05B104", "C1525"},
	})
	if _, err := auditReleaseBOM(bad, components); err == nil || !strings.Contains(err.Error(), "footprint") {
		t.Fatalf("wrong footprint must fail, got %v", err)
	}
}

func TestAuditReleaseBOMFailsClosedOnRequiredFieldsAndPopulationDrift(t *testing.T) {
	components := []releaseComponent{
		{Designator: "C1", AddIntoBOM: true, Footprint: "C0402", Manufacturer: "MPN-C1", Supplier: "C1525"},
		{Designator: "R1", AddIntoBOM: true, Footprint: "R0402", Manufacturer: "MPN-R1", Supplier: "C11702"},
	}
	tests := []struct {
		name    string
		records [][]string
		want    string
	}{
		{
			name: "missing supplier column",
			records: [][]string{
				{"Quantity", "Designator", "Footprint", "Manufacturer Part"},
				{"1", "C1", "C0402", "MPN-C1"},
			},
			want: "missing required column",
		},
		{
			name: "missing live population",
			records: [][]string{
				{"Quantity", "Designator", "Footprint", "Manufacturer Part", "Supplier Part"},
				{"1", "C1", "C0402", "MPN-C1", "C1525"},
			},
			want: "missing=[R1]",
		},
		{
			name: "manufacturer part mismatch",
			records: [][]string{
				{"Quantity", "Designator", "Footprint", "Manufacturer Part", "Supplier Part"},
				{"1", "C1", "C0402", "WRONG-MPN", "C1525"},
				{"1", "R1", "R0402", "MPN-R1", "C11702"},
			},
			want: "manufacturer part",
		},
		{
			name: "LCSC part mismatch",
			records: [][]string{
				{"Quantity", "Designator", "Footprint", "Manufacturer Part", "Supplier Part"},
				{"1", "C1", "C0402", "MPN-C1", "C999"},
				{"1", "R1", "R0402", "MPN-R1", "C11702"},
			},
			want: "LCSC part",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := releaseTestCSV(t, releaseCSVUTF8, ',', tc.records)
			if _, err := auditReleaseBOM(data, components); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q refusal, got %v", tc.want, err)
			}
		})
	}
}

func TestAuditAndFilterReleaseCPLKeepsOnlyLiveSMT(t *testing.T) {
	components := []releaseComponent{
		{Designator: "C1", Layer: 1, AddIntoBOM: true, Footprint: "C0402", X: 100, Y: 200, Rotation: -90, SMT: true},
		{Designator: "J1", Layer: 1, AddIntoBOM: true, Footprint: "CONN-TH", X: 300, Y: 400, SMT: false},
		{Designator: "R2", Layer: 1, AddIntoBOM: false, Footprint: "R0402", X: 500, Y: 600, SMT: true},
	}
	data := releaseTestCSV(t, releaseCSVUTF16LE, '\t', [][]string{
		{"Designator", "Footprint", "Mid X", "Mid Y", "Layer", "Rotation", "SMD"},
		{"C1", "C0402", "2.540mm", "5.080mm", "T", "270", "Yes"},
		{"J1", "CONN-TH", "7.620mm", "10.160mm", "T", "0", "No"},
		{"R2", "R0402", "12.700mm", "15.240mm", "T", "0", "Yes"},
	})
	filtered, inspection, err := auditAndFilterReleaseCPL(data, components)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(filtered, []byte{0xff, 0xfe}) {
		t.Fatal("filtered CPL must preserve UTF-16LE encoding")
	}
	if !reflect.DeepEqual(inspection.References, []string{"C1"}) || !reflect.DeepEqual(inspection.Excluded, []string{"J1", "R2"}) {
		t.Fatalf("unexpected CPL inspection: %+v", inspection)
	}
	refs, err := releaseCSVReferences(filtered)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(refs, []string{"C1"}) {
		t.Fatalf("filtered CPL refs = %v", refs)
	}

	badUnit := releaseTestCSV(t, releaseCSVUTF8, ',', [][]string{
		{"Designator", "Footprint", "Mid X", "Mid Y", "Layer", "Rotation", "SMD"},
		{"C1", "C0402", "2.540", "5.080mm", "T", "0", "Yes"},
	})
	if _, _, err := auditAndFilterReleaseCPL(badUnit, components[:1]); err == nil || !strings.Contains(err.Error(), "explicitly in mm") {
		t.Fatalf("unitless coordinate must fail, got %v", err)
	}
}

func TestAuditReleaseCPLFailsClosedOnRequiredFieldsAndPopulationDrift(t *testing.T) {
	components := []releaseComponent{
		{Designator: "C1", Layer: 1, AddIntoBOM: true, Footprint: "C0402", X: 100, Y: 200, Rotation: 0, SMT: true},
		{Designator: "R1", Layer: 2, AddIntoBOM: true, Footprint: "R0402", X: 300, Y: 400, Rotation: 90, SMT: true},
	}
	header := []string{"Designator", "Footprint", "Mid X", "Mid Y", "Layer", "Rotation", "SMD"}
	validC1 := []string{"C1", "C0402", "2.540mm", "5.080mm", "Top", "0", "Yes"}
	validR1 := []string{"R1", "R0402", "7.620mm", "10.160mm", "Bottom", "90", "Yes"}
	tests := []struct {
		name    string
		records [][]string
		want    string
	}{
		{
			name: "missing rotation column",
			records: [][]string{
				{"Designator", "Footprint", "Mid X", "Mid Y", "Layer", "SMD"},
				{"C1", "C0402", "2.540mm", "5.080mm", "Top", "Yes"},
			},
			want: "missing required column",
		},
		{
			name:    "missing live SMT population",
			records: [][]string{header, validC1},
			want:    "missing=[R1]",
		},
		{
			name: "footprint mismatch",
			records: [][]string{header,
				{"C1", "C0603", "2.540mm", "5.080mm", "Top", "0", "Yes"}, validR1},
			want: "footprint",
		},
		{
			name: "position mismatch",
			records: [][]string{header,
				{"C1", "C0402", "2.800mm", "5.080mm", "Top", "0", "Yes"}, validR1},
			want: "position",
		},
		{
			name: "layer mismatch",
			records: [][]string{header, validC1,
				{"R1", "R0402", "7.620mm", "10.160mm", "Top", "90", "Yes"}},
			want: "does not match live PCB layer",
		},
		{
			name: "rotation mismatch",
			records: [][]string{header, validC1,
				{"R1", "R0402", "7.620mm", "10.160mm", "Bottom", "0", "Yes"}},
			want: "rotation",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := releaseTestCSV(t, releaseCSVUTF8, ',', tc.records)
			if _, _, err := auditAndFilterReleaseCPL(data, components); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q refusal, got %v", tc.want, err)
			}
		})
	}
}

func TestInspectReleaseGerberZIPEnforcesLiveBoardExpectations(t *testing.T) {
	data := releaseTestZIP(t, true)
	inspection, err := inspectReleaseGerberZIPForBoard(data, releaseGerberExpectations{
		CopperLayers: 2, NeedsPTH: true, TopPaste: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.CopperLayerCount != 2 || len(inspection.SolderMaskFiles) != 2 || len(inspection.SilkscreenFiles) != 2 {
		t.Fatalf("unexpected Gerber inspection: %+v", inspection)
	}
	if _, err := inspectReleaseGerberZIPForBoard(data, releaseGerberExpectations{CopperLayers: 4}); err == nil || !strings.Contains(err.Error(), "requires 4") {
		t.Fatalf("missing inner copper must fail, got %v", err)
	}
	if _, err := inspectReleaseGerberZIPForBoard(data, releaseGerberExpectations{CopperLayers: 2, NeedsNPTH: true}); err == nil || !strings.Contains(err.Error(), "NPTH") {
		t.Fatalf("missing NPTH must fail, got %v", err)
	}
	if _, err := inspectReleaseGerberZIPForBoard(data, releaseGerberExpectations{CopperLayers: 2, BottomPaste: true}); err == nil || !strings.Contains(err.Error(), "bottom paste") {
		t.Fatalf("missing bottom paste must fail, got %v", err)
	}
	innerGerber := "G04 inner copper*\n%FSLAX46Y46*%\n%MOMM*%\nX100Y100D02*\nX200Y200D01*\nM02*\n"
	missingTopCopper := rewriteReleaseTestZIP(t,
		[]string{"Gerber_TopLayer.GTL"},
		map[string]string{"Gerber_InnerLayer1.G1": innerGerber},
	)
	if _, err := inspectReleaseGerberZIPForBoard(missingTopCopper, releaseGerberExpectations{CopperLayers: 2}); err == nil || !strings.Contains(err.Error(), "top or bottom copper") {
		t.Fatalf("missing top copper must fail even when layer count still matches, got %v", err)
	}
	missingBottomMask := rewriteReleaseTestZIP(t, []string{"Gerber_BottomSolderMaskLayer.GBS"}, nil)
	if _, err := inspectReleaseGerberZIPForBoard(missingBottomMask, releaseGerberExpectations{CopperLayers: 2}); err == nil || !strings.Contains(err.Error(), "solder-mask") {
		t.Fatalf("missing bottom solder mask must fail, got %v", err)
	}
	missingPTH := rewriteReleaseTestZIP(t, []string{"Drill_PTH_Through.DRL"}, nil)
	if _, err := inspectReleaseGerberZIPForBoard(missingPTH, releaseGerberExpectations{CopperLayers: 2, NeedsPTH: true}); err == nil || !strings.Contains(err.Error(), "PTH") {
		t.Fatalf("missing PTH must fail, got %v", err)
	}
}
