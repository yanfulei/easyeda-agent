package app

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/spec"
)

const (
	releaseExportTimeout   = 150 * time.Second
	releaseArtifactMaxSize = 512 << 20
	releaseZipEntryMaxSize = 128 << 20
	releaseZipTotalMaxSize = 512 << 20
)

type manufacturingReleaseOptions struct {
	window     string
	outDir     string
	specPath   string
	drcTimeout time.Duration
}

// manufacturingReleaseRuntime is the small I/O seam around the composite.
// Production uses the existing CLI paths; tests replace the functions with a
// deterministic in-memory editor without weakening the release checks.
type manufacturingReleaseRuntime struct {
	discoverDocs func(*appConfig, string) ([]openableDoc, string, string, error)
	acquireLease func(*appConfig, string, string, string) (string, error)
	releaseLease func(*appConfig, string) error
	reload       func(*appConfig, string, string) (string, error)
	waitSettle   func(*appConfig, string, string) bool
	request      func(*appConfig, string, string, any, time.Duration) (*actionResult, error)
	pcbCheck     func(*appConfig, string, float64, *spec.Spec, io.Writer) (*pcbCheckReport, error)
	now          func() time.Time
}

var liveManufacturingReleaseRuntime = manufacturingReleaseRuntime{
	discoverDocs: discoverDocs,
	acquireLease: func(cfg *appConfig, window, documentUUID, documentType string) (string, error) {
		lease, err := acquireManufacturingWriteLease(cfg, window, documentUUID, documentType, 0)
		if err != nil {
			return "", err
		}
		return lease.LeaseID, nil
	},
	releaseLease: releaseManufacturingWriteLease,
	reload:       reloadDocumentByUUID,
	waitSettle:   waitDocSettleFor,
	request:      requestActionTimed,
	pcbCheck:     gatherPcbCheckReport,
	now:          time.Now,
}

type releaseBinding struct {
	ProjectUUID  string `json:"projectUuid"`
	ProjectName  string `json:"projectName"`
	DocumentUUID string `json:"documentUuid"`
	DocumentName string `json:"documentName"`
	DocumentType string `json:"documentType"`
}

type releaseComponent struct {
	PrimitiveID  string  `json:"primitiveId"`
	Designator   string  `json:"designator"`
	Layer        int     `json:"layer"`
	AddIntoBOM   bool    `json:"addIntoBom"`
	Manufacturer string  `json:"manufacturerId,omitempty"`
	Supplier     string  `json:"supplierId,omitempty"`
	Footprint    string  `json:"footprint,omitempty"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
	Rotation     float64 `json:"rotation"`
	SMT          bool    `json:"smt"`
}

type releaseGerberInspection struct {
	Entries          []string `json:"entries"`
	GerberFiles      []string `json:"gerberFiles"`
	CopperFiles      []string `json:"copperFiles"`
	SolderMaskFiles  []string `json:"solderMaskFiles"`
	SilkscreenFiles  []string `json:"silkscreenFiles"`
	PasteMaskFiles   []string `json:"pasteMaskFiles,omitempty"`
	DrillFiles       []string `json:"drillFiles"`
	PTHDrillFiles    []string `json:"pthDrillFiles,omitempty"`
	NPTHDrillFiles   []string `json:"npthDrillFiles,omitempty"`
	OutlineFiles     []string `json:"outlineFiles"`
	CopperLayerCount int      `json:"copperLayerCount"`
}

type releaseArtifactManifest struct {
	Kind     string `json:"kind"`
	File     string `json:"file"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

type releaseCheckManifest struct {
	SaveReload struct {
		Passed bool `json:"passed"`
	} `json:"saveReload"`
	PCBCheck struct {
		Passed   bool `json:"passed"`
		Findings int  `json:"findings"`
		Errors   int  `json:"errors"`
		Warnings int  `json:"warnings"`
	} `json:"pcbCheck"`
	NativeDRC struct {
		Passed     bool           `json:"passed"`
		Violations int            `json:"violations"`
		Counts     map[string]int `json:"counts"`
	} `json:"nativeDrc"`
}

type releaseReferenceAudit struct {
	BOMExpected []string             `json:"bomExpected"`
	CPLExpected []string             `json:"cplExpected"`
	BOM         releaseBOMInspection `json:"bom"`
	CPL         releaseCPLInspection `json:"cpl"`
}

type releaseSnapshotObservation struct {
	Stage  string `json:"stage"`
	SHA256 string `json:"sha256"`
}

type releaseSnapshotManifest struct {
	SchemaVersion  string                       `json:"schemaVersion"`
	BaselineSHA256 string                       `json:"baselineSha256"`
	Stable         bool                         `json:"stable"`
	Observations   []releaseSnapshotObservation `json:"observations"`
}

type releaseSpecManifest struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Strict bool   `json:"strict"`
	raw    []byte
}

type manufacturingReleaseManifest struct {
	SchemaVersion  string                    `json:"schemaVersion"`
	CreatedAt      string                    `json:"createdAt"`
	Binding        releaseBinding            `json:"binding"`
	Checks         releaseCheckManifest      `json:"checks"`
	Snapshot       releaseSnapshotManifest   `json:"snapshot"`
	Components     []releaseComponent        `json:"components"`
	References     releaseReferenceAudit     `json:"references"`
	Gerber         releaseGerberInspection   `json:"gerber"`
	Artifacts      []releaseArtifactManifest `json:"artifacts"`
	Spec           *releaseSpecManifest      `json:"spec,omitempty"`
	OrderSubmitted bool                      `json:"orderSubmitted"`
}

type releaseFile struct {
	Kind     string
	Name     string
	MimeType string
	Data     []byte
}

func newManufacturingCmd(cfg *appConfig, stdout, stderr io.Writer) *cobra.Command {
	var window string
	root := &cobra.Command{
		Use:   "manufacturing",
		Short: "Validate and package manufacturing deliverables (never places an order)",
	}
	root.PersistentFlags().StringVar(&window, "window", "", "EasyEDA window ID (normally use --project)")

	var outDir, specPath string
	var drcTimeoutSec int
	release := &cobra.Command{
		Use:   "release-bundle",
		Short: "Create a checked Gerber/BOM/CPL bundle with a SHA256 manifest",
		Long: `Create a fail-closed manufacturing release from one pinned PCB document.

The command saves and reloads the PCB, runs the reconstructed PCB check and the
native EasyEDA DRC, exports Gerber/BOM/CPL, validates the ZIP and CSV contents,
and reconciles BOM/CPL reference designators against the live PCB component set.
Only after every gate passes does it atomically publish a release directory.
It never opens an order page, submits an order, or handles payment.`,
		Args: cobra.NoArgs,
		Example: `  easyeda --project power-probe --doc PCB1 manufacturing release-bundle
  easyeda --project power-probe --doc PCB1 manufacturing release-bundle --out-dir release/rev-a
  easyeda --project power-probe --doc PCB1 manufacturing release-bundle --spec .easyeda/s0-power-probe.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runManufacturingRelease(cfg, manufacturingReleaseOptions{
				window: window, outDir: outDir, specPath: specPath,
				drcTimeout: time.Duration(drcTimeoutSec) * time.Second,
			}, stdout, stderr)
		},
	}
	release.Flags().StringVar(&outDir, "out-dir", "", "new release directory (default: .easyeda/releases/<pcb>-<utc>)")
	release.Flags().StringVar(&specPath, "spec", "", "required S0 spec JSON; release refuses missing or non-strict design intent")
	release.Flags().IntVar(&drcTimeoutSec, "drc-timeout", 180, "seconds to wait for native PCB DRC")
	root.AddCommand(release)
	return root
}

func runManufacturingRelease(cfg *appConfig, opts manufacturingReleaseOptions, stdout, stderr io.Writer) error {
	return runManufacturingReleaseWithRuntime(cfg, opts, stdout, stderr, liveManufacturingReleaseRuntime)
}

func runManufacturingReleaseWithRuntime(cfg *appConfig, opts manufacturingReleaseOptions, stdout, stderr io.Writer, rt manufacturingReleaseRuntime) (retErr error) {
	if strings.TrimSpace(cfg.project) == "" {
		return fmt.Errorf("manufacturing release-bundle requires --project")
	}
	if strings.TrimSpace(cfg.doc) == "" {
		return fmt.Errorf("manufacturing release-bundle requires --doc naming one PCB")
	}
	if opts.drcTimeout <= 0 {
		return fmt.Errorf("--drc-timeout must be greater than zero")
	}
	if strings.TrimSpace(opts.specPath) == "" {
		return fmt.Errorf("manufacturing release-bundle requires --spec with the approved S0 design intent")
	}

	checkSpec, specManifest, err := loadReleaseSpec(opts.specPath)
	if err != nil {
		return err
	}

	docs, _, win, err := rt.discoverDocs(cfg, opts.window)
	if err != nil {
		return fmt.Errorf("resolve release document: %w", err)
	}
	target, err := resolveDoc(docs, cfg.doc)
	if err != nil {
		return fmt.Errorf("resolve release PCB %q: %w", cfg.doc, err)
	}
	if !strings.EqualFold(target.Type, "pcb") {
		return fmt.Errorf("release target %q is %s, not a PCB", target.Name, target.Type)
	}

	// Use the immutable UUID for every subsequent guard, even when the caller used
	// a friendly document name. The live response context is checked after every
	// action, so foreground drift can never publish a bundle for the wrong board.
	pinned := *cfg
	pinned.doc = target.UUID
	pinned.advisories = &actionAdvisoryCollector{}
	if rt.acquireLease == nil || rt.releaseLease == nil {
		return fmt.Errorf("manufacturing release runtime has no write-lease controls")
	}
	leaseID, err := rt.acquireLease(&pinned, win, target.UUID, "pcb")
	if err != nil {
		return fmt.Errorf("acquire manufacturing write lease: %w", err)
	}
	defer func() {
		if releaseErr := rt.releaseLease(&pinned, leaseID); releaseErr != nil {
			if retErr == nil {
				retErr = fmt.Errorf("release manufacturing write lease: %w", releaseErr)
			} else {
				retErr = fmt.Errorf("%w (also failed to release manufacturing write lease: %v)", retErr, releaseErr)
			}
		}
	}()
	docType, err := rt.reload(&pinned, win, target.UUID)
	if err != nil {
		return fmt.Errorf("save/reload release PCB: %w", err)
	}
	if !strings.EqualFold(docType, "pcb") {
		return fmt.Errorf("reload returned document type %q for PCB %s", docType, target.UUID)
	}
	if !rt.waitSettle(&pinned, win, "pcb") {
		return fmt.Errorf("reloaded PCB %s did not settle; refusing manufacturing release", target.UUID)
	}
	if err := pinned.advisories.error(); err != nil {
		return fmt.Errorf("save/reload release PCB: %w", err)
	}

	cur, err := rt.request(&pinned, "document.current", win, nil, defaultActionTimeout)
	if err != nil {
		return fmt.Errorf("read PCB context after reload: %w", err)
	}
	binding, err := bindReleaseContext(cur, target, cfg.project)
	if err != nil {
		return err
	}
	var abandonedBaseline *int
	requestBound := func(action string, payload any, timeout time.Duration) (*actionResult, error) {
		res, callErr := rt.request(&pinned, action, win, payload, timeout)
		if callErr != nil {
			return nil, fmt.Errorf("%s: %w", action, callErr)
		}
		if contextErr := verifyReleaseContext(action, res, binding); contextErr != nil {
			return nil, contextErr
		}
		if reliabilityErr := verifyReleaseReliability(action, res, &abandonedBaseline); reliabilityErr != nil {
			return nil, reliabilityErr
		}
		return res, nil
	}

	var baseline *releaseManufacturingSnapshot
	observations := make([]releaseSnapshotObservation, 0, 10)
	observeSnapshot := func(stage string) error {
		res, snapshotErr := requestBound("pcb.manufacturing.snapshot", nil, releaseExportTimeout)
		if snapshotErr != nil {
			return snapshotErr
		}
		snapshot, snapshotErr := parseReleaseManufacturingSnapshot(res.Result)
		if snapshotErr != nil {
			return fmt.Errorf("%s manufacturing snapshot: %w", stage, snapshotErr)
		}
		if baseline == nil {
			baseline = snapshot
		} else if snapshot.SHA256 != baseline.SHA256 {
			return fmt.Errorf("PCB manufacturing state drifted at %s: baseline=%s observed=%s; retry from a stable document",
				stage, baseline.SHA256, snapshot.SHA256)
		}
		observations = append(observations, releaseSnapshotObservation{Stage: stage, SHA256: snapshot.SHA256})
		return nil
	}
	if err := observeSnapshot("pcb-check.before"); err != nil {
		return err
	}

	// gatherPcbCheckReport historically degrades a missing optional data source to
	// a warning. A manufacturing release cannot equate "not checked" with zero
	// findings, so any such warning blocks publication.
	var checkDiagnostics bytes.Buffer
	checkWriter := io.MultiWriter(stderr, &checkDiagnostics)
	checkReport, err := rt.pcbCheck(&pinned, win, 3.0, checkSpec, checkWriter)
	if err != nil {
		return fmt.Errorf("pcb check did not complete: %w", err)
	}
	if diagnostic := strings.TrimSpace(checkDiagnostics.String()); diagnostic != "" {
		return fmt.Errorf("pcb check was incomplete and cannot release: %s", diagnostic)
	}
	if err := pinned.advisories.error(); err != nil {
		return fmt.Errorf("pcb check reliability gate: %w", err)
	}
	if checkReport == nil {
		return fmt.Errorf("pcb check returned no report")
	}
	if checkReport.Summary.Errors < 0 || checkReport.Summary.Warnings < 0 ||
		checkReport.Summary.Total < 0 || checkReport.Summary.Total != len(checkReport.Findings) {
		return fmt.Errorf("pcb check returned an inconsistent summary (total=%d findings=%d errors=%d warnings=%d)",
			checkReport.Summary.Total, len(checkReport.Findings), checkReport.Summary.Errors, checkReport.Summary.Warnings)
	}
	if checkReport.Summary.Errors != 0 || checkReport.Summary.Warnings != 0 {
		return fmt.Errorf("pcb check failed release gate: %d error(s), %d warning(s)",
			checkReport.Summary.Errors, checkReport.Summary.Warnings)
	}
	if !checkReport.Passed {
		return fmt.Errorf("pcb check returned passed:false with zero summarized findings")
	}
	if err := observeSnapshot("pcb-check.after"); err != nil {
		return err
	}

	if err := observeSnapshot("native-drc.before"); err != nil {
		return err
	}
	drcRes, err := requestBound("pcb.drc.check", map[string]any{"strict": true}, opts.drcTimeout)
	if err != nil {
		return err
	}
	violations, hasViolations := drcRes.Result["violations"].([]any)
	passed, hasPassed := drcRes.Result["passed"].(bool)
	if !hasViolations || !hasPassed {
		return fmt.Errorf("native PCB DRC returned an incomplete result (passed/violations missing)")
	}
	drcReport := flattenDrcResult(drcRes.Result)
	if !passed || !drcReport.Passed || len(violations) != 0 || drcReport.Total != 0 {
		return fmt.Errorf("native PCB DRC failed release gate: %d violation(s)", drcReport.Total)
	}
	if err := observeSnapshot("native-drc.after"); err != nil {
		return err
	}

	if err := observeSnapshot("gerber-export.before"); err != nil {
		return err
	}
	gerberRes, err := requestBound("pcb.export.gerber", map[string]any{
		"fileName": "gerber.zip", "unit": "mm",
	}, releaseExportTimeout)
	if err != nil {
		return err
	}
	if err := observeSnapshot("gerber-export.after"); err != nil {
		return err
	}
	if err := observeSnapshot("bom-export.before"); err != nil {
		return err
	}
	bomRes, err := requestBound("pcb.export.bom", map[string]any{
		"fileName": "bom.csv", "fileType": "csv",
	}, releaseExportTimeout)
	if err != nil {
		return err
	}
	if err := observeSnapshot("bom-export.after"); err != nil {
		return err
	}
	if err := observeSnapshot("cpl-export.before"); err != nil {
		return err
	}
	cplRes, err := requestBound("pcb.export.pick_and_place", map[string]any{
		"fileName": "cpl.csv", "fileType": "csv", "unit": "mm",
	}, releaseExportTimeout)
	if err != nil {
		return err
	}
	if err := observeSnapshot("cpl-export.after"); err != nil {
		return err
	}

	gerber, err := releaseFileFromAction("gerber", "gerber.zip", "application/zip", gerberRes)
	if err != nil {
		return err
	}
	bom, err := releaseFileFromAction("bom", "bom.csv", "text/csv", bomRes)
	if err != nil {
		return err
	}
	cpl, err := releaseFileFromAction("cpl", "cpl.csv", "text/csv", cplRes)
	if err != nil {
		return err
	}
	gerberInspection, err := inspectReleaseGerberZIPForBoard(gerber.Data, releaseGerberExpectations{
		CopperLayers: baseline.CopperLayerCount,
		NeedsPTH:     baseline.NeedsPTH, NeedsNPTH: baseline.NeedsNPTH,
		TopPaste: baseline.TopSMT, BottomPaste: baseline.BottomSMT,
	})
	if err != nil {
		return fmt.Errorf("Gerber validation: %w", err)
	}
	bomInspection, err := auditReleaseBOM(bom.Data, baseline.Components)
	if err != nil {
		return fmt.Errorf("BOM validation: %w", err)
	}
	filteredCPL, cplInspection, err := auditAndFilterReleaseCPL(cpl.Data, baseline.Components)
	if err != nil {
		return fmt.Errorf("CPL validation: %w", err)
	}
	cpl.Data = filteredCPL
	var snapshotArtifact bytes.Buffer
	if err := json.Indent(&snapshotArtifact, baseline.CanonicalJSON, "", "  "); err != nil {
		return fmt.Errorf("format manufacturing snapshot artifact: %w", err)
	}
	snapshotArtifact.WriteByte('\n')
	snapshotFile := releaseFile{
		Kind: "manufacturing-snapshot", Name: "manufacturing-snapshot.json",
		MimeType: "application/json", Data: snapshotArtifact.Bytes(),
	}
	specFile := releaseFile{
		Kind: "design-spec", Name: specManifest.File,
		MimeType: "application/json", Data: specManifest.raw,
	}

	createdAt := rt.now().UTC()
	manifest := manufacturingReleaseManifest{
		SchemaVersion: "easyeda.manufacturing.release/v2",
		CreatedAt:     createdAt.Format(time.RFC3339Nano),
		Binding:       binding,
		Snapshot: releaseSnapshotManifest{
			SchemaVersion: releaseSnapshotSchema, BaselineSHA256: baseline.SHA256,
			Stable: true, Observations: observations,
		},
		Components: baseline.Components,
		References: releaseReferenceAudit{
			BOMExpected: baseline.BOMReferences, CPLExpected: baseline.CPLReferences,
			BOM: bomInspection, CPL: cplInspection,
		},
		Gerber:         gerberInspection,
		Spec:           specManifest,
		OrderSubmitted: false,
	}
	manifest.Checks.SaveReload.Passed = true
	manifest.Checks.PCBCheck.Passed = true
	manifest.Checks.PCBCheck.Findings = checkReport.Summary.Total
	manifest.Checks.PCBCheck.Errors = checkReport.Summary.Errors
	manifest.Checks.PCBCheck.Warnings = checkReport.Summary.Warnings
	manifest.Checks.NativeDRC.Passed = true
	manifest.Checks.NativeDRC.Violations = drcReport.Total
	manifest.Checks.NativeDRC.Counts = drcReport.Counts

	finalDir, manifestPath, err := publishManufacturingRelease(opts.outDir, target.Name, createdAt,
		[]releaseFile{gerber, bom, cpl, snapshotFile, specFile}, &manifest)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{
		"ok":             true,
		"releaseDir":     finalDir,
		"manifestPath":   manifestPath,
		"manifest":       manifest,
		"orderSubmitted": false,
	})
}

func bindReleaseContext(res *actionResult, target openableDoc, projectFallback string) (releaseBinding, error) {
	if res == nil || res.Context == nil {
		return releaseBinding{}, fmt.Errorf("document.current returned no live response context")
	}
	c := res.Context
	if c.ProjectUUID == "" {
		return releaseBinding{}, fmt.Errorf("document.current returned no project UUID; cannot bind release")
	}
	if c.DocumentUUID != target.UUID || !strings.EqualFold(c.DocumentType, "pcb") {
		return releaseBinding{}, fmt.Errorf("release target drifted: expected PCB %s, got %s (%s)", target.UUID, c.DocumentUUID, c.DocumentType)
	}
	projectName := c.ProjectName
	if projectName == "" {
		projectName = projectFallback
	}
	return releaseBinding{
		ProjectUUID: c.ProjectUUID, ProjectName: projectName,
		DocumentUUID: target.UUID, DocumentName: target.Name, DocumentType: "pcb",
	}, nil
}

func verifyReleaseContext(action string, res *actionResult, want releaseBinding) error {
	if res == nil || res.Context == nil {
		return fmt.Errorf("%s returned no live document context; refusing release", action)
	}
	c := res.Context
	if c.ProjectUUID != want.ProjectUUID || c.DocumentUUID != want.DocumentUUID || !strings.EqualFold(c.DocumentType, want.DocumentType) {
		return fmt.Errorf("%s ran against the wrong context: expected project %s PCB %s, got project %s %s %s",
			action, want.ProjectUUID, want.DocumentUUID, c.ProjectUUID, c.DocumentType, c.DocumentUUID)
	}
	return nil
}

func verifyReleaseReliability(action string, res *actionResult, abandonedBaseline **int) error {
	if res == nil {
		return fmt.Errorf("%s returned no response", action)
	}
	if len(res.Warnings) != 0 {
		return fmt.Errorf("%s returned warning(s): %s", action, strings.Join(res.Warnings, "; "))
	}
	if res.StaleRisk != "" {
		return fmt.Errorf("%s returned staleRisk: %s", action, res.StaleRisk)
	}
	if res.ConcurrentWriter != "" {
		return fmt.Errorf("%s detected a concurrent writer: %s", action, res.ConcurrentWriter)
	}
	if !res.Seq.Known {
		return fmt.Errorf("%s returned no FIFO sequence evidence; update the connector before release", action)
	}
	if res.Seq.Unordered {
		return fmt.Errorf("%s ran through the unordered diagnostic bypass; refusing release", action)
	}
	if *abandonedBaseline == nil {
		value := res.Seq.SeqAbandoned
		*abandonedBaseline = &value
	} else if res.Seq.SeqAbandoned != **abandonedBaseline {
		return fmt.Errorf("%s observed abandoned-action drift from %d to %d; a timed-out handler may still mutate the board",
			action, **abandonedBaseline, res.Seq.SeqAbandoned)
	}
	return nil
}

func loadReleaseSpec(path string) (*spec.Spec, *releaseSpecManifest, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil, fmt.Errorf("release spec path is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read release spec: %w", err)
	}
	parsed, err := spec.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse release spec: %w", err)
	}
	issues := spec.Validate(parsed)
	counts := issueCounts(issues)
	if counts["ERROR"] != 0 || counts["WARN"] != 0 {
		return nil, nil, fmt.Errorf("release spec failed strict validation: %d error(s), %d warning(s)", counts["ERROR"], counts["WARN"])
	}
	return parsed, &releaseSpecManifest{
		File: "design-spec.json", SHA256: releaseSHA256(raw), Strict: true, raw: raw,
	}, nil
}

func releaseFileFromAction(kind, name, mime string, res *actionResult) (releaseFile, error) {
	if res == nil || len(res.Artifacts) != 1 {
		return releaseFile{}, fmt.Errorf("%s export returned %d artifacts; expected exactly one", kind, lenArtifacts(res))
	}
	source := res.Artifacts[0].Path
	if source == "" {
		return releaseFile{}, fmt.Errorf("%s export artifact has no persisted path", kind)
	}
	info, err := os.Stat(source)
	if err != nil {
		return releaseFile{}, fmt.Errorf("stat %s artifact: %w", kind, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return releaseFile{}, fmt.Errorf("%s artifact is not a non-empty regular file", kind)
	}
	if info.Size() > releaseArtifactMaxSize {
		return releaseFile{}, fmt.Errorf("%s artifact exceeds %d bytes", kind, releaseArtifactMaxSize)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return releaseFile{}, fmt.Errorf("read %s artifact: %w", kind, err)
	}
	if int64(len(data)) != info.Size() {
		return releaseFile{}, fmt.Errorf("%s artifact changed while being read", kind)
	}
	return releaseFile{Kind: kind, Name: name, MimeType: mime, Data: data}, nil
}

func lenArtifacts(res *actionResult) int {
	if res == nil {
		return 0
	}
	return len(res.Artifacts)
}

type releaseGerberExpectations struct {
	CopperLayers int
	NeedsPTH     bool
	NeedsNPTH    bool
	TopPaste     bool
	BottomPaste  bool
}

func inspectReleaseGerberZIP(data []byte) (releaseGerberInspection, error) {
	return inspectReleaseGerberZIPForBoard(data, releaseGerberExpectations{NeedsPTH: true})
}

func inspectReleaseGerberZIPForBoard(data []byte, expected releaseGerberExpectations) (releaseGerberInspection, error) {
	if len(data) < 4 || !bytes.Equal(data[:4], []byte{'P', 'K', 3, 4}) {
		return releaseGerberInspection{}, fmt.Errorf("artifact is not a non-empty ZIP archive")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return releaseGerberInspection{}, fmt.Errorf("open ZIP: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > 2048 {
		return releaseGerberInspection{}, fmt.Errorf("ZIP has invalid entry count %d", len(zr.File))
	}
	inspection := releaseGerberInspection{}
	var total uint64
	for _, file := range zr.File {
		if file.FileInfo().IsDir() {
			continue
		}
		if file.UncompressedSize64 == 0 || file.UncompressedSize64 > releaseZipEntryMaxSize {
			return releaseGerberInspection{}, fmt.Errorf("ZIP entry %q has invalid size %d", file.Name, file.UncompressedSize64)
		}
		total += file.UncompressedSize64
		if total > releaseZipTotalMaxSize {
			return releaseGerberInspection{}, fmt.Errorf("ZIP expands beyond %d bytes", releaseZipTotalMaxSize)
		}
		rc, err := file.Open()
		if err != nil {
			return releaseGerberInspection{}, fmt.Errorf("open ZIP entry %q: %w", file.Name, err)
		}
		content, readErr := io.ReadAll(io.LimitReader(rc, releaseZipEntryMaxSize+1))
		closeErr := rc.Close()
		if readErr != nil || closeErr != nil || len(content) == 0 || len(content) > releaseZipEntryMaxSize {
			return releaseGerberInspection{}, fmt.Errorf("read/CRC validation failed for ZIP entry %q", file.Name)
		}
		name := filepath.ToSlash(file.Name)
		inspection.Entries = append(inspection.Entries, name)
		lower := strings.ToLower(name)
		if isReleaseDrillName(lower) {
			if !looksLikeExcellon(content) {
				return releaseGerberInspection{}, fmt.Errorf("drill entry %q is not valid-looking Excellon", name)
			}
			inspection.DrillFiles = append(inspection.DrillFiles, name)
			if strings.Contains(lower, "npth") || strings.Contains(lower, "nonplated") || strings.Contains(lower, "non-plated") {
				inspection.NPTHDrillFiles = append(inspection.NPTHDrillFiles, name)
			} else if strings.Contains(lower, "pth") || strings.Contains(lower, "plated") || strings.Contains(lower, "through") {
				inspection.PTHDrillFiles = append(inspection.PTHDrillFiles, name)
			}
			continue
		}
		if isReleaseGerberName(lower) {
			if !looksLikeGerber(content) {
				return releaseGerberInspection{}, fmt.Errorf("Gerber entry %q has no recognizable RS-274X commands", name)
			}
			inspection.GerberFiles = append(inspection.GerberFiles, name)
			switch {
			case isReleaseCopperName(lower):
				inspection.CopperFiles = append(inspection.CopperFiles, name)
			case isReleaseSolderMaskName(lower):
				inspection.SolderMaskFiles = append(inspection.SolderMaskFiles, name)
			case isReleaseSilkscreenName(lower):
				inspection.SilkscreenFiles = append(inspection.SilkscreenFiles, name)
			case isReleasePasteMaskName(lower):
				inspection.PasteMaskFiles = append(inspection.PasteMaskFiles, name)
			}
			if isReleaseOutlineName(lower) {
				inspection.OutlineFiles = append(inspection.OutlineFiles, name)
			}
		}
	}
	inspection.CopperLayerCount = len(inspection.CopperFiles)
	for _, list := range [][]string{
		inspection.Entries, inspection.GerberFiles, inspection.CopperFiles,
		inspection.SolderMaskFiles, inspection.SilkscreenFiles, inspection.PasteMaskFiles,
		inspection.DrillFiles, inspection.PTHDrillFiles, inspection.NPTHDrillFiles,
		inspection.OutlineFiles,
	} {
		sort.Strings(list)
	}
	if len(inspection.GerberFiles) == 0 {
		return releaseGerberInspection{}, fmt.Errorf("ZIP contains no recognized Gerber files")
	}
	if expected.CopperLayers == 0 && expected.NeedsPTH && len(inspection.DrillFiles) == 0 {
		return releaseGerberInspection{}, fmt.Errorf("ZIP contains no recognized drill file")
	}
	if len(inspection.OutlineFiles) == 0 {
		return releaseGerberInspection{}, fmt.Errorf("ZIP contains no recognized board-outline Gerber")
	}
	if expected.CopperLayers > 0 {
		if len(inspection.CopperFiles) != expected.CopperLayers {
			return releaseGerberInspection{}, fmt.Errorf("ZIP has %d copper layer file(s), live PCB requires %d: %v",
				len(inspection.CopperFiles), expected.CopperLayers, inspection.CopperFiles)
		}
		if !releaseHasTopBottom(inspection.CopperFiles) {
			return releaseGerberInspection{}, fmt.Errorf("ZIP is missing top or bottom copper Gerber")
		}
		if !releaseHasTopBottom(inspection.SolderMaskFiles) {
			return releaseGerberInspection{}, fmt.Errorf("ZIP is missing top or bottom solder-mask Gerber")
		}
		if !releaseHasTopBottom(inspection.SilkscreenFiles) {
			return releaseGerberInspection{}, fmt.Errorf("ZIP is missing top or bottom silkscreen Gerber")
		}
		if expected.TopPaste && !releaseHasSide(inspection.PasteMaskFiles, true) {
			return releaseGerberInspection{}, fmt.Errorf("ZIP is missing top paste-mask Gerber for top-side SMT components")
		}
		if expected.BottomPaste && !releaseHasSide(inspection.PasteMaskFiles, false) {
			return releaseGerberInspection{}, fmt.Errorf("ZIP is missing bottom paste-mask Gerber for bottom-side SMT components")
		}
		if expected.NeedsPTH && len(inspection.PTHDrillFiles) == 0 {
			return releaseGerberInspection{}, fmt.Errorf("ZIP is missing a PTH drill file required by live plated holes/vias")
		}
		if expected.NeedsNPTH && len(inspection.NPTHDrillFiles) == 0 {
			return releaseGerberInspection{}, fmt.Errorf("ZIP is missing an NPTH drill file required by live non-plated holes")
		}
	}
	return inspection, nil
}

func isReleaseGerberName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".gbr", ".ger", ".gtl", ".gbl", ".gts", ".gbs", ".gto", ".gbo", ".gtp", ".gbp", ".gko", ".gml", ".gm1", ".gp1", ".gp2":
		return true
	default:
		if len(ext) > 2 && strings.HasPrefix(ext, ".g") {
			_, err := strconv.Atoi(ext[2:])
			return err == nil
		}
		return false
	}
}

func isReleaseCopperName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".gtl" || ext == ".gbl" || strings.Contains(name, "innerlayer") || strings.Contains(name, "copperlayer") {
		return true
	}
	if (strings.HasPrefix(ext, ".g") || strings.HasPrefix(ext, ".gp")) && len(ext) > 2 {
		_, err := strconv.Atoi(strings.TrimPrefix(strings.TrimPrefix(ext, ".gp"), ".g"))
		return err == nil
	}
	return strings.Contains(name, "toplayer") || strings.Contains(name, "bottomlayer")
}

func isReleaseSolderMaskName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".gts" || ext == ".gbs" || strings.Contains(name, "soldermask")
}

func isReleaseSilkscreenName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".gto" || ext == ".gbo" || strings.Contains(name, "silkscreen") || strings.Contains(name, "silk")
}

func isReleasePasteMaskName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".gtp" || ext == ".gbp" || strings.Contains(name, "pastemask") || strings.Contains(name, "paste")
}

func releaseHasTopBottom(files []string) bool {
	return releaseHasSide(files, true) && releaseHasSide(files, false)
}

func releaseHasSide(files []string, top bool) bool {
	for _, file := range files {
		lower := strings.ToLower(file)
		ext := strings.ToLower(filepath.Ext(lower))
		if top && (strings.Contains(lower, "top") || ext == ".gtl" || ext == ".gts" || ext == ".gto" || ext == ".gtp") {
			return true
		}
		if !top && (strings.Contains(lower, "bottom") || ext == ".gbl" || ext == ".gbs" || ext == ".gbo" || ext == ".gbp") {
			return true
		}
	}
	return false
}

func isReleaseDrillName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".drl" || ext == ".xln" || (ext == ".txt" && strings.Contains(name, "drill"))
}

func isReleaseOutlineName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".gko" || ext == ".gml" || ext == ".gm1" ||
		strings.Contains(name, "outline") || strings.Contains(name, "profile")
}

func looksLikeGerber(data []byte) bool {
	upper := strings.ToUpper(string(data))
	hasHeader := strings.Contains(upper, "%FS") || strings.Contains(upper, "%MO") || strings.Contains(upper, "G04")
	hasDraw := strings.Contains(upper, "D01*") || strings.Contains(upper, "D02*") || strings.Contains(upper, "M02*")
	return hasHeader && hasDraw
}

func looksLikeExcellon(data []byte) bool {
	upper := strings.ToUpper(string(data))
	return strings.Contains(upper, "M48") &&
		(strings.Contains(upper, "M30") || strings.Contains(upper, "T01") || strings.Contains(upper, "T1"))
}

func releaseCSVReferences(data []byte) ([]string, error) {
	table, err := parseReleaseCSVTable(data)
	if err != nil {
		return nil, err
	}
	refs, found, err := referencesFromCSVRecords(table.Records)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no Designator/Reference column found")
	}
	return refs, nil
}

func referencesFromCSVRecords(records [][]string) ([]string, bool, error) {
	headerRow, refColumn := -1, -1
	for rowIndex, row := range records {
		for columnIndex, field := range row {
			if releaseReferenceHeader(field) {
				headerRow, refColumn = rowIndex, columnIndex
				break
			}
		}
		if refColumn >= 0 {
			break
		}
	}
	if refColumn < 0 {
		return nil, false, nil
	}
	seen := map[string]bool{}
	for rowIndex, row := range records[headerRow+1:] {
		if csvRowBlank(row) {
			continue
		}
		if refColumn >= len(row) {
			return nil, true, fmt.Errorf("CSV row %d has no reference column", headerRow+rowIndex+2)
		}
		for _, token := range strings.FieldsFunc(row[refColumn], func(r rune) bool {
			return r == ',' || r == ';' || unicode.IsSpace(r)
		}) {
			ref := normalizeReleaseReference(token)
			if ref != "" {
				seen[ref] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil, true, fmt.Errorf("reference column contains no designators")
	}
	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, true, nil
}

func releaseReferenceHeader(value string) bool {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, strings.TrimPrefix(strings.TrimSpace(value), "\ufeff"))
	switch normalized {
	case "designator", "designators", "reference", "references", "referencedesignator", "refdes", "ref":
		return true
	default:
		return false
	}
}

func csvRowBlank(row []string) bool {
	for _, value := range row {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

func compareReleaseReferences(label string, expected, actual []string) error {
	missing, extra := releaseReferenceDiff(expected, actual)
	if len(missing) != 0 || len(extra) != 0 {
		return fmt.Errorf("%s reference designators do not match the PCB: missing=%v extra=%v", label, missing, extra)
	}
	return nil
}

func releaseReferenceDiff(expected, actual []string) (missing, extra []string) {
	want, got := map[string]bool{}, map[string]bool{}
	for _, ref := range expected {
		want[normalizeReleaseReference(ref)] = true
	}
	for _, ref := range actual {
		got[normalizeReleaseReference(ref)] = true
	}
	for ref := range want {
		if !got[ref] {
			missing = append(missing, ref)
		}
	}
	for ref := range got {
		if !want[ref] {
			extra = append(extra, ref)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

func normalizeReleaseReference(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func publishManufacturingRelease(outDir, documentName string, createdAt time.Time, files []releaseFile, manifest *manufacturingReleaseManifest) (string, string, error) {
	if manifest == nil {
		return "", "", fmt.Errorf("release manifest is nil")
	}
	finalDir := strings.TrimSpace(outDir)
	if finalDir == "" {
		root, ok := artifactOutputDir()
		if !ok {
			return "", "", fmt.Errorf("resolve project output directory")
		}
		stamp := createdAt.UTC().Format("20060102T150405.000000000Z")
		finalDir = filepath.Join(root, ".easyeda", "releases", releaseSafeName(documentName)+"-"+stamp)
	}
	abs, err := filepath.Abs(finalDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve release directory: %w", err)
	}
	finalDir = filepath.Clean(abs)
	if _, err := os.Lstat(finalDir); err == nil {
		return "", "", fmt.Errorf("release directory already exists: %s", finalDir)
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("inspect release directory: %w", err)
	}
	parent := filepath.Dir(finalDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", "", fmt.Errorf("create release parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".release-tmp-")
	if err != nil {
		return "", "", fmt.Errorf("create release staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()

	manifest.Artifacts = make([]releaseArtifactManifest, 0, len(files))
	seenNames := map[string]bool{}
	for _, file := range files {
		if len(file.Data) == 0 {
			return "", "", fmt.Errorf("release file %s is empty", file.Kind)
		}
		if filepath.Base(file.Name) != file.Name || seenNames[file.Name] {
			return "", "", fmt.Errorf("invalid or duplicate release file name %q", file.Name)
		}
		seenNames[file.Name] = true
		if err := writeReleaseFile(filepath.Join(staging, file.Name), file.Data); err != nil {
			return "", "", err
		}
		manifest.Artifacts = append(manifest.Artifacts, releaseArtifactManifest{
			Kind: file.Kind, File: file.Name, MimeType: file.MimeType,
			Size: int64(len(file.Data)), SHA256: releaseSHA256(file.Data),
		})
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("encode release manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')
	if err := writeReleaseFile(filepath.Join(staging, "manifest.json"), manifestData); err != nil {
		return "", "", err
	}
	if err := os.Rename(staging, finalDir); err != nil {
		return "", "", fmt.Errorf("publish release directory: %w", err)
	}
	published = true
	if dir, openErr := os.Open(parent); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return finalDir, filepath.Join(finalDir, "manifest.json"), nil
}

func writeReleaseFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create release file %s: %w", path, err)
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("write release file %s: %w", path, err)
	}
	if closeErr != nil {
		return fmt.Errorf("close release file %s: %w", path, closeErr)
	}
	return nil
}

func releaseSafeName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "pcb"
	}
	var out strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	name := strings.Trim(out.String(), "._-")
	if name == "" {
		return "pcb"
	}
	return name
}

func releaseSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
