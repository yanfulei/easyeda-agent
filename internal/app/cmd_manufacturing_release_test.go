package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

func releaseTestZIP(t *testing.T, includeDrill bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := map[string]string{
		"Gerber_TopLayer.GTL":              "G04 top copper*\n%FSLAX46Y46*%\n%MOMM*%\nX100Y100D02*\nX200Y200D01*\nM02*\n",
		"Gerber_BottomLayer.GBL":           "G04 bottom copper*\n%FSLAX46Y46*%\n%MOMM*%\nX100Y100D02*\nX200Y200D01*\nM02*\n",
		"Gerber_TopSolderMaskLayer.GTS":    "G04 top mask*\n%FSLAX46Y46*%\n%MOMM*%\nX100Y100D02*\nX200Y200D01*\nM02*\n",
		"Gerber_BottomSolderMaskLayer.GBS": "G04 bottom mask*\n%FSLAX46Y46*%\n%MOMM*%\nX100Y100D02*\nX200Y200D01*\nM02*\n",
		"Gerber_TopSilkscreenLayer.GTO":    "G04 top silk*\n%FSLAX46Y46*%\n%MOMM*%\nX100Y100D02*\nX200Y200D01*\nM02*\n",
		"Gerber_BottomSilkscreenLayer.GBO": "G04 bottom silk*\n%FSLAX46Y46*%\n%MOMM*%\nX100Y100D02*\nX200Y200D01*\nM02*\n",
		"Gerber_TopPasteMaskLayer.GTP":     "G04 top paste*\n%FSLAX46Y46*%\n%MOMM*%\nX100Y100D02*\nX200Y200D01*\nM02*\n",
		"Gerber_BoardOutlineLayer.GKO":     "G04 board outline*\n%FSLAX46Y46*%\n%MOMM*%\nX0Y0D02*\nX100Y100D01*\nM02*\n",
	}
	if includeDrill {
		entries["Drill_PTH_Through.DRL"] = "M48\nMETRIC,TZ\nT01C0.800\n%\nT01\nX100Y100\nM30\n"
	}
	for name, content := range entries {
		writer, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func rewriteReleaseTestZIP(t *testing.T, remove []string, add map[string]string) []byte {
	t.Helper()
	source := releaseTestZIP(t, true)
	zr, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, name := range remove {
		removed[name] = true
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeEntry := func(name string, data []byte) {
		writer, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range zr.File {
		if removed[file.Name] {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(rc)
		closeErr := rc.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %s: read=%v close=%v", file.Name, readErr, closeErr)
		}
		writeEntry(file.Name, data)
	}
	for name, data := range add {
		writeEntry(name, []byte(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInspectReleaseGerberZIPRequiresRealGerberDrillAndOutline(t *testing.T) {
	inspection, err := inspectReleaseGerberZIP(releaseTestZIP(t, true))
	if err != nil {
		t.Fatalf("valid bundle rejected: %v", err)
	}
	if len(inspection.GerberFiles) != 8 || len(inspection.DrillFiles) != 1 || len(inspection.OutlineFiles) != 1 {
		t.Fatalf("unexpected inspection: %+v", inspection)
	}
	if _, err := inspectReleaseGerberZIP([]byte{'P', 'K', 3, 4}); err == nil {
		t.Fatal("four-byte ZIP prefix must not pass release validation")
	}
	if _, err := inspectReleaseGerberZIP(releaseTestZIP(t, false)); err == nil || !strings.Contains(err.Error(), "drill") {
		t.Fatalf("missing drill should fail explicitly, got %v", err)
	}
}

func releaseTestSnapshot() map[string]any {
	component := func(id, ref, footprint, mpn, lcsc string, xMM, yMM float64) map[string]any {
		return map[string]any{
			"primitiveId": id, "designator": ref, "layer": float64(1), "side": "top",
			"addIntoBom": true, "manufacturerId": mpn, "supplierId": lcsc,
			"footprint": map[string]any{"name": footprint},
			"x":         xMM / 0.0254, "y": yMM / 0.0254, "rotation": float64(0),
			"pads": []any{
				map[string]any{"primitiveId": id + "-1", "layer": float64(1), "hole": nil, "metallization": false},
				map[string]any{"primitiveId": id + "-2", "layer": float64(1), "hole": nil, "metallization": false},
			},
		}
	}
	components := []any{
		component("p-c1", "C1", "C0402", "MPN-C1", "C1525", 1, 1),
		component("p-r1", "R1", "R0402", "MPN-R1", "C11702", 2, 2),
	}
	return map[string]any{
		"schemaVersion": releaseSnapshotSchema, "complete": true,
		"board": map[string]any{"uuid": "board-1"}, "pcb": map[string]any{"uuid": "pcb-uuid"},
		"components": components,
		"pads": []any{
			components[0].(map[string]any)["pads"].([]any)[0], components[0].(map[string]any)["pads"].([]any)[1],
			components[1].(map[string]any)["pads"].([]any)[0], components[1].(map[string]any)["pads"].([]any)[1],
		},
		"lines": []any{}, "arcs": []any{}, "polylines": []any{}, "vias": []any{},
		"pours": []any{}, "poured": []any{}, "fills": []any{}, "regions": []any{},
		"strings": []any{}, "attributes": []any{}, "images": []any{}, "objects": []any{},
		"dimensions": []any{}, "layers": []any{map[string]any{"id": float64(1)}, map[string]any{"id": float64(2)}},
		"copperLayerCount": float64(2), "nets": []any{}, "drcRules": map[string]any{"configurationName": "JLC-2L"},
	}
}

func TestReleaseCSVReferencesHandlesGroupedBOMAndRejectsDiff(t *testing.T) {
	bom := []byte("\xef\xbb\xbfComment,Designator,Footprint\n100nF,\"C1, C2\",0603\n10k,R1,0603\n")
	refs, err := releaseCSVReferences(bom)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"C1", "C2", "R1"}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
	if err := compareReleaseReferences("BOM", want, refs); err != nil {
		t.Fatal(err)
	}
	err = compareReleaseReferences("CPL", want, []string{"C1", "R9"})
	if err == nil || !strings.Contains(err.Error(), "missing=[C2 R1]") || !strings.Contains(err.Error(), "extra=[R9]") {
		t.Fatalf("unexpected reference diff: %v", err)
	}
}

func writeReleaseTestArtifact(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const releaseTestSpecJSON = `{
  "board":{"outline":"compact"},
  "stackup":{"layers":2},
  "assembly":{"profile":"reflow","side":"top"},
  "modules":[
    {"name":"POWER","kind":"POWER","zone":"left","parts":["C1"]},
    {"name":"IO","kind":"IO","zone":"right","parts":["R1"]}
  ],
  "flow":["POWER","IO"],
  "flowAxis":"x"
}`

type releaseNegativeOptions struct {
	mutateResponse func(action string, ordinal int, res *actionResult)
	mutateSnapshot func(observation int, snapshot map[string]any)
	checkReport    *pcbCheckReport
	checkErr       error
	releaseErr     error
	stderr         *bytes.Buffer
}

func runReleaseNegativeHarness(t *testing.T, options releaseNegativeOptions) (error, string, []string) {
	t.Helper()
	tmp := t.TempDir()
	specPath := writeReleaseTestArtifact(t, tmp, "s0.json", []byte(releaseTestSpecJSON))
	outDir := filepath.Join(tmp, "release")
	context := &actionContext{
		ProjectUUID: "project-uuid", ProjectName: "POWER_PROBE",
		DocumentUUID: "pcb-uuid", DocumentType: "pcb", TabID: "tab-1",
	}
	seq, snapshotObservation := 0, 0
	ordinals := map[string]int{}
	var actions []string
	gerberPath := writeReleaseTestArtifact(t, tmp, "source-gerber.zip", releaseTestZIP(t, true))
	bomPath := writeReleaseTestArtifact(t, tmp, "source-bom.csv", []byte("Quantity,Designator,Footprint,Manufacturer Part,Supplier Part\n1,C1,C0402,MPN-C1,C1525\n1,R1,R0402,MPN-R1,C11702\n"))
	cplPath := writeReleaseTestArtifact(t, tmp, "source-cpl.csv", []byte("Designator,Footprint,Mid X,Mid Y,Layer,Rotation,SMD\nC1,C0402,1mm,1mm,Top,0,Yes\nR1,R0402,2mm,2mm,Top,0,Yes\n"))
	rt := manufacturingReleaseRuntime{
		discoverDocs: func(*appConfig, string) ([]openableDoc, string, string, error) {
			return []openableDoc{{UUID: "pcb-uuid", Type: "pcb", Name: "PCB1"}}, "", "w1", nil
		},
		acquireLease: func(*appConfig, string, string, string) (string, error) { return "lease-1", nil },
		releaseLease: func(*appConfig, string) error { return options.releaseErr },
		reload:       func(*appConfig, string, string) (string, error) { return "pcb", nil },
		waitSettle:   func(*appConfig, string, string) bool { return true },
		request: func(_ *appConfig, action, _ string, _ any, _ time.Duration) (*actionResult, error) {
			actions = append(actions, action)
			seq++
			ordinals[action]++
			res := &actionResult{
				OK: true, Context: context, Result: map[string]any{},
				Seq: schSeqCounters{Known: true, Seq: seq, SeqAbandoned: 0},
			}
			if action == "document.current" {
				res.Seq.Unordered = true
			}
			switch action {
			case "pcb.manufacturing.snapshot":
				snapshotObservation++
				res.Result = releaseTestSnapshot()
				if options.mutateSnapshot != nil {
					options.mutateSnapshot(snapshotObservation, res.Result)
				}
			case "pcb.drc.check":
				res.Result = map[string]any{"passed": true, "violations": []any{}}
			case "pcb.export.gerber":
				res.Artifacts = []artifactRef{{Path: gerberPath, FileName: "gerber.zip"}}
			case "pcb.export.bom":
				res.Artifacts = []artifactRef{{Path: bomPath, FileName: "bom.csv"}}
			case "pcb.export.pick_and_place":
				res.Artifacts = []artifactRef{{Path: cplPath, FileName: "cpl.csv"}}
			}
			if options.mutateResponse != nil {
				options.mutateResponse(action, ordinals[action], res)
			}
			return res, nil
		},
		pcbCheck: func(*appConfig, string, float64, *spec.Spec, io.Writer) (*pcbCheckReport, error) {
			if options.checkErr != nil {
				return nil, options.checkErr
			}
			if options.checkReport != nil {
				return options.checkReport, nil
			}
			return &pcbCheckReport{Passed: true, Summary: pcbCheckSummary{}}, nil
		},
		now: func() time.Time { return time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC) },
	}
	var stdout, stderr bytes.Buffer
	err := runManufacturingReleaseWithRuntime(
		&appConfig{project: "POWER_PROBE", doc: "PCB1"},
		manufacturingReleaseOptions{outDir: outDir, specPath: specPath, drcTimeout: 30 * time.Second},
		&stdout, &stderr, rt,
	)
	if options.stderr != nil {
		options.stderr.Write(stderr.Bytes())
	}
	return err, outDir, actions
}

func assertReleaseNotPublished(t *testing.T, outDir string) {
	t.Helper()
	if _, err := os.Lstat(outDir); !os.IsNotExist(err) {
		t.Fatalf("release directory must not be published: path=%s err=%v", outDir, err)
	}
}

func TestRunManufacturingReleasePublishesOnlyAfterAllGates(t *testing.T) {
	tmp := t.TempDir()
	specData := []byte(`{
  "board":{"outline":"compact"},
  "stackup":{"layers":2},
  "assembly":{"profile":"reflow","side":"top"},
  "modules":[
    {"name":"POWER","kind":"POWER","zone":"left","parts":["C1"]},
    {"name":"IO","kind":"IO","zone":"right","parts":["R1"]}
  ],
  "flow":["POWER","IO"],
  "flowAxis":"x"
}`)
	specPath := writeReleaseTestArtifact(t, tmp, "s0.json", specData)
	gerberPath := writeReleaseTestArtifact(t, tmp, "source-gerber.zip", releaseTestZIP(t, true))
	bomPath := writeReleaseTestArtifact(t, tmp, "source-bom.csv", []byte("Quantity,Designator,Footprint,Manufacturer Part,Supplier Part\n1,C1,C0402,MPN-C1,C1525\n1,R1,R0402,MPN-R1,C11702\n"))
	cplPath := writeReleaseTestArtifact(t, tmp, "source-cpl.csv", []byte("Designator,Footprint,Mid X,Mid Y,Layer,Rotation,SMD\nC1,C0402,1mm,1mm,Top,0,Yes\nR1,R0402,2mm,2mm,Top,0,Yes\n"))
	outDir := filepath.Join(tmp, "release")

	context := &actionContext{
		ProjectUUID: "project-uuid", ProjectName: "POWER_PROBE",
		DocumentUUID: "pcb-uuid", DocumentType: "pcb", TabID: "tab-1",
	}
	snapshotResult := releaseTestSnapshot()
	var sequence []string
	seq := 0
	rt := manufacturingReleaseRuntime{
		discoverDocs: func(*appConfig, string) ([]openableDoc, string, string, error) {
			sequence = append(sequence, "discover")
			return []openableDoc{{UUID: "pcb-uuid", Type: "pcb", Name: "PCB1"}}, "", "w1", nil
		},
		acquireLease: func(_ *appConfig, window, documentUUID, documentType string) (string, error) {
			sequence = append(sequence, "lease.acquire:"+window+":"+documentUUID+":"+documentType)
			return "lease-1", nil
		},
		releaseLease: func(_ *appConfig, leaseID string) error {
			sequence = append(sequence, "lease.release:"+leaseID)
			return nil
		},
		reload: func(_ *appConfig, win, target string) (string, error) {
			sequence = append(sequence, "reload:"+win+":"+target)
			return "pcb", nil
		},
		waitSettle: func(*appConfig, string, string) bool {
			sequence = append(sequence, "settle")
			return true
		},
		request: func(_ *appConfig, action, _ string, _ any, _ time.Duration) (*actionResult, error) {
			sequence = append(sequence, action)
			seq++
			res := &actionResult{OK: true, Context: context, Result: map[string]any{}, Seq: schSeqCounters{
				Known: true, Seq: seq, SeqAbandoned: 0,
			}}
			if action == "document.current" {
				res.Seq.Unordered = true
			}
			switch action {
			case "pcb.manufacturing.snapshot":
				res.Result = snapshotResult
			case "pcb.drc.check":
				res.Result = map[string]any{"passed": true, "violations": []any{}}
			case "pcb.export.gerber":
				res.Artifacts = []artifactRef{{Path: gerberPath, FileName: "gerber.zip"}}
			case "pcb.export.bom":
				res.Artifacts = []artifactRef{{Path: bomPath, FileName: "bom.csv"}}
			case "pcb.export.pick_and_place":
				res.Artifacts = []artifactRef{{Path: cplPath, FileName: "cpl.csv"}}
			}
			return res, nil
		},
		pcbCheck: func(*appConfig, string, float64, *spec.Spec, io.Writer) (*pcbCheckReport, error) {
			sequence = append(sequence, "pcb.check")
			return &pcbCheckReport{Passed: true, Summary: pcbCheckSummary{}}, nil
		},
		now: func() time.Time { return time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC) },
	}

	cfg := &appConfig{project: "POWER_PROBE", doc: "PCB1"}
	var stdout, stderr bytes.Buffer
	err := runManufacturingReleaseWithRuntime(cfg, manufacturingReleaseOptions{
		outDir: outDir, specPath: specPath, drcTimeout: 30 * time.Second,
	}, &stdout, &stderr, rt)
	if err != nil {
		t.Fatalf("release failed: %v\nstderr: %s", err, stderr.String())
	}
	wantSequence := []string{
		"discover", "lease.acquire:w1:pcb-uuid:pcb", "reload:w1:pcb-uuid", "settle", "document.current",
		"pcb.manufacturing.snapshot", "pcb.check", "pcb.manufacturing.snapshot",
		"pcb.manufacturing.snapshot", "pcb.drc.check", "pcb.manufacturing.snapshot",
		"pcb.manufacturing.snapshot", "pcb.export.gerber", "pcb.manufacturing.snapshot",
		"pcb.manufacturing.snapshot", "pcb.export.bom", "pcb.manufacturing.snapshot",
		"pcb.manufacturing.snapshot", "pcb.export.pick_and_place", "pcb.manufacturing.snapshot",
		"lease.release:lease-1",
	}
	if !reflect.DeepEqual(sequence, wantSequence) {
		t.Fatalf("sequence = %v, want %v", sequence, wantSequence)
	}
	for _, name := range []string{"gerber.zip", "bom.csv", "cpl.csv", "manufacturing-snapshot.json", "design-spec.json", "manifest.json"} {
		if info, err := os.Stat(filepath.Join(outDir, name)); err != nil || info.Size() == 0 {
			t.Fatalf("published %s: info=%v err=%v", name, info, err)
		}
	}
	manifestRaw, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest manufacturingReleaseManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Binding.DocumentUUID != "pcb-uuid" || !manifest.References.BOM.Matched || !manifest.References.CPL.Matched ||
		!manifest.Snapshot.Stable || len(manifest.Snapshot.Observations) != 10 || len(manifest.Artifacts) != 5 || manifest.OrderSubmitted {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if manifest.Spec == nil || manifest.Spec.File != "design-spec.json" || manifest.Spec.SHA256 != releaseSHA256(specData) {
		t.Fatalf("release spec was not embedded and hashed: %+v", manifest.Spec)
	}
	for _, artifact := range manifest.Artifacts {
		data, err := os.ReadFile(filepath.Join(outDir, artifact.File))
		if err != nil {
			t.Fatal(err)
		}
		if artifact.SHA256 != releaseSHA256(data) {
			t.Fatalf("bad SHA256 for %s", artifact.File)
		}
	}
	var response map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || response["orderSubmitted"] != false {
		t.Fatalf("bad command response: %s err=%v", stdout.String(), err)
	}
}

func TestRunManufacturingReleaseRequiresSpecBeforeTouchingEditor(t *testing.T) {
	discoveryCalled := false
	rt := manufacturingReleaseRuntime{
		discoverDocs: func(*appConfig, string) ([]openableDoc, string, string, error) {
			discoveryCalled = true
			return nil, "", "", nil
		},
	}
	err := runManufacturingReleaseWithRuntime(
		&appConfig{project: "POWER_PROBE", doc: "PCB1"},
		manufacturingReleaseOptions{drcTimeout: 30 * time.Second},
		io.Discard, io.Discard, rt,
	)
	if err == nil || !strings.Contains(err.Error(), "requires --spec") {
		t.Fatalf("missing spec must fail explicitly, got %v", err)
	}
	if discoveryCalled {
		t.Fatal("missing spec must fail before editor discovery or mutation")
	}
}

func TestRunManufacturingReleaseFailsClosedOnReliabilityEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*actionResult)
		want   string
	}{
		{name: "warnings", mutate: func(res *actionResult) { res.Warnings = []string{"inventory partial"} }, want: "warning(s)"},
		{name: "stale risk", mutate: func(res *actionResult) { res.StaleRisk = "cached response" }, want: "staleRisk"},
		{name: "concurrent writer", mutate: func(res *actionResult) { res.ConcurrentWriter = "window-2" }, want: "concurrent writer"},
		{name: "abandoned action drift", mutate: func(res *actionResult) { res.Seq.SeqAbandoned = 1 }, want: "abandoned-action drift"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err, outDir, actions := runReleaseNegativeHarness(t, releaseNegativeOptions{
				mutateResponse: func(action string, ordinal int, res *actionResult) {
					if action == "pcb.manufacturing.snapshot" && ordinal == 1 {
						tc.mutate(res)
					}
				},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q refusal, got %v (actions=%v)", tc.want, err, actions)
			}
			assertReleaseNotPublished(t, outDir)
		})
	}
}

func TestRunManufacturingReleaseRejectsSnapshotDrift(t *testing.T) {
	err, outDir, actions := runReleaseNegativeHarness(t, releaseNegativeOptions{
		mutateSnapshot: func(observation int, snapshot map[string]any) {
			if observation == 2 {
				snapshot["lines"] = []any{map[string]any{
					"primitiveId": "track-drift", "startX": float64(0), "endX": float64(42),
				}}
			}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "manufacturing state drifted at pcb-check.after") {
		t.Fatalf("snapshot drift must block release, got %v (actions=%v)", err, actions)
	}
	assertReleaseNotPublished(t, outDir)
}

func TestRunManufacturingReleaseRejectsFailedOrIncompletePCBCheck(t *testing.T) {
	tests := []struct {
		name      string
		report    *pcbCheckReport
		checkErr  error
		wantError string
	}{
		{
			name:      "passed false without summarized findings",
			report:    &pcbCheckReport{Passed: false, Summary: pcbCheckSummary{}},
			wantError: "passed:false",
		},
		{
			name: "error finding",
			report: &pcbCheckReport{
				Passed:   false,
				Summary:  pcbCheckSummary{Errors: 1, Total: 1},
				Findings: []pcbCheckFinding{{Message: "unrouted power net"}},
			},
			wantError: "1 error(s)",
		},
		{
			name:      "checker error",
			checkErr:  errors.New("inventory unavailable"),
			wantError: "pcb check did not complete",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err, outDir, actions := runReleaseNegativeHarness(t, releaseNegativeOptions{
				checkReport: tc.report, checkErr: tc.checkErr,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected %q refusal, got %v (actions=%v)", tc.wantError, err, actions)
			}
			assertReleaseNotPublished(t, outDir)
		})
	}
}

func TestRunManufacturingReleaseAllowsInfoOnlyPCBCheck(t *testing.T) {
	report := &pcbCheckReport{
		Passed:   true,
		Summary:  pcbCheckSummary{Total: 1},
		Findings: []pcbCheckFinding{{Type: "fiducial-missing", Level: "INFO", Message: "panel rail may provide fiducials"}},
	}
	err, outDir, actions := runReleaseNegativeHarness(t, releaseNegativeOptions{checkReport: report})
	if err != nil {
		t.Fatalf("INFO-only check must not block manufacturing release: %v (actions=%v)", err, actions)
	}
	if _, err := os.Stat(filepath.Join(outDir, "manifest.json")); err != nil {
		t.Fatalf("INFO-only release was not published: %v", err)
	}
}

func TestRunManufacturingReleaseDoesNotTurnPublishedBundleIntoFailureOnLeaseCleanupError(t *testing.T) {
	var stderr bytes.Buffer
	err, outDir, _ := runReleaseNegativeHarness(t, releaseNegativeOptions{
		releaseErr: errors.New("daemon unavailable"),
		stderr:     &stderr,
	})
	if err != nil {
		t.Fatalf("published release must remain successful when lease cleanup fails: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "manifest.json")); statErr != nil {
		t.Fatalf("published bundle is missing after cleanup warning: %v", statErr)
	}
	if !strings.Contains(stderr.String(), "release bundle was published") || !strings.Contains(stderr.String(), "lease cleanup failed") {
		t.Fatalf("lease cleanup warning was not explicit: %q", stderr.String())
	}
}

func TestPublishManufacturingReleaseRefusesExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	manifest := &manufacturingReleaseManifest{}
	_, _, err := publishManufacturingRelease(dir, "PCB1", time.Now(), []releaseFile{{Kind: "bom", Name: "bom.csv", Data: []byte("x")}}, manifest)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected existing-directory refusal, got %v", err)
	}
}
