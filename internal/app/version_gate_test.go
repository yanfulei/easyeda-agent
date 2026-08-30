package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/version"
)

// findingFor returns the finding for component (first match) or a zero value.
func findingFor(t *testing.T, rep versionGateReport, component string) versionFinding {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Component == component {
			return f
		}
	}
	t.Fatalf("no %s finding in %+v", component, rep.Findings)
	return versionFinding{}
}

func TestEvaluateVersionGateDaemonGrading(t *testing.T) {
	cases := []struct {
		name       string
		cli        string
		daemon     string
		wantSev    string
		wantInMsg  string
		wantVerdct string
	}{
		{
			// The exact case users reported (#181): a PATCH-level daemon drift.
			// It must BLOCK — daemon and CLI ship as one version, so any
			// difference can only mean a stale process.
			name: "patch drift blocks", cli: "v1.1.1", daemon: "v1.1.0",
			wantSev: versionSevBlock, wantInMsg: "v1.1.0", wantVerdct: versionSevBlock,
		},
		{
			name: "minor drift blocks", cli: "v1.2.0", daemon: "v1.1.0",
			wantSev: versionSevBlock, wantVerdct: versionSevBlock,
		},
		{
			// A newer daemon than CLI is just as much a mismatch (stale CLI on PATH).
			name: "daemon ahead blocks", cli: "v1.1.0", daemon: "v1.2.0",
			wantSev: versionSevBlock, wantVerdct: versionSevBlock,
		},
		{
			name: "same clean release is ok", cli: "v1.1.1", daemon: "v1.1.1",
			wantSev: versionSevOK, wantVerdct: versionSevOK,
		},
		{
			name: "leading v optional", cli: "1.1.1", daemon: "v1.1.1",
			wantSev: versionSevOK, wantVerdct: versionSevOK,
		},
		{
			name: "daemon version missing is skipped", cli: "v1.1.1", daemon: "",
			wantSev: versionSevSkipped, wantVerdct: versionSevSkipped,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := evaluateVersionGate(c.cli, c.daemon, nil)
			got := findingFor(t, rep, "daemon")
			if got.Severity != c.wantSev {
				t.Fatalf("severity = %q, want %q (reason: %s)", got.Severity, c.wantSev, got.Reason)
			}
			if rep.Verdict != c.wantVerdct {
				t.Fatalf("verdict = %q, want %q", rep.Verdict, c.wantVerdct)
			}
			if c.wantInMsg != "" && !strings.Contains(got.Reason, c.wantInMsg) {
				t.Fatalf("reason %q missing %q", got.Reason, c.wantInMsg)
			}
			if c.wantSev == versionSevBlock && !strings.Contains(got.Fix, "make dev") {
				t.Fatalf("blocking daemon finding must name the restart path, got fix: %q", got.Fix)
			}
		})
	}
}

// TestVersionGateDevStampExemption is the load-bearing regression: `make dev`
// (air) stamps the binary with git describe (v1.1.1-19-g…-dirty), whose semver
// core is an OLD tag. CLAUDE.md mandates such a build is treated as "dev" and
// never false-flagged. Breaking this makes development impossible.
func TestVersionGateDevStampExemption(t *testing.T) {
	cases := []struct {
		name    string
		cli     string
		daemon  string
		conn    string
		wantSev map[string]string
	}{
		{
			name: "dev cli vs older clean daemon", cli: "v1.1.1-19-ge9552d8-dirty", daemon: "v1.1.0",
			conn:    "1.1.0",
			wantSev: map[string]string{"daemon": versionSevSkipped, "connector": versionSevSkipped},
		},
		{
			name: "clean cli vs dev daemon", cli: "v1.1.1", daemon: "v1.1.1-3-gabc1234",
			wantSev: map[string]string{"daemon": versionSevSkipped},
		},
		{
			name: "literal dev string", cli: "dev", daemon: "dev",
			conn:    "1.1.1",
			wantSev: map[string]string{"daemon": versionSevSkipped, "connector": versionSevSkipped},
		},
		{
			name: "identical dev stamps", cli: "v1.1.1-19-ge9552d8-dirty", daemon: "v1.1.1-19-ge9552d8-dirty",
			wantSev: map[string]string{"daemon": versionSevSkipped},
		},
		{
			// Two DIFFERENT dev stamps is not a false flag — it is hard evidence
			// of two builds. Still never blocks.
			name: "differing dev stamps warn only", cli: "v1.1.1-19-ge9552d8", daemon: "v1.1.1-3-gabc1234",
			wantSev: map[string]string{"daemon": versionSevWarn},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var conns []string
			if c.conn != "" {
				conns = []string{c.conn}
			}
			rep := evaluateVersionGate(c.cli, c.daemon, conns)
			for comp, want := range c.wantSev {
				if got := findingFor(t, rep, comp); got.Severity != want {
					t.Fatalf("%s severity = %q, want %q (reason: %s)", comp, got.Severity, want, got.Reason)
				}
			}
			if rep.Verdict == versionSevBlock {
				t.Fatalf("dev stamp must never block, got verdict=block: %+v", rep.Findings)
			}
			if len(rep.blockingFindings()) != 0 {
				t.Fatalf("dev stamp produced blocking findings: %+v", rep.blockingFindings())
			}
		})
	}
}

func TestEvaluateVersionGateConnectorGrading(t *testing.T) {
	cases := []struct {
		name    string
		cli     string
		conn    string
		wantSev string
	}{
		{"patch behind blocks", "v1.1.1", "1.1.0", versionSevBlock},
		{"patch ahead blocks", "v1.1.0", "1.1.1", versionSevBlock},
		{"minor behind blocks", "v1.2.0", "1.1.0", versionSevBlock},
		{"major behind blocks", "v2.0.0", "1.9.9", versionSevBlock},
		{"same version ok", "v1.1.1", "1.1.1", versionSevOK},
		{"unparseable connector skipped", "v1.1.1", "not-a-version", versionSevSkipped},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := evaluateVersionGate(c.cli, c.cli, []string{c.conn})
			got := findingFor(t, rep, "connector")
			if got.Severity != c.wantSev {
				t.Fatalf("severity = %q, want %q (reason: %s)", got.Severity, c.wantSev, got.Reason)
			}
			if c.wantSev == versionSevBlock || c.wantSev == versionSevWarn {
				// The connector fix must spell the two hard-won steps: uninstall
				// first (same uuid → silent import failure) and fully relaunch
				// EasyEDA (re-import does not reload open windows).
				for _, want := range []string{"卸载", "完全退出并重启 EasyEDA"} {
					if !strings.Contains(got.Fix, want) {
						t.Fatalf("connector fix %q missing %q", got.Fix, want)
					}
				}
			}
		})
	}
}

func TestEvaluateVersionGateDedupsConnectors(t *testing.T) {
	rep := evaluateVersionGate("v1.1.1", "v1.1.1", []string{"1.1.1", "1.1.1", "", "1.0.0"})
	if len(rep.Connectors) != 2 {
		t.Fatalf("connectors = %v, want 2 distinct non-empty", rep.Connectors)
	}
	// Worst-wins: one aligned window must not mask the stale one.
	if rep.Verdict != versionSevBlock {
		t.Fatalf("verdict = %q, want block (a stale peer window is present)", rep.Verdict)
	}
}

func TestVersionGateFromHealth(t *testing.T) {
	raw := []byte(`{"service":"easyeda-agent","version":"1.1.0","windows":[
	  {"windowId":"w1","connectorVersion":"1.1.0"},
	  {"windowId":"w2","connectorVersion":"1.1.0"}]}`)
	rep := versionGateFromHealth(raw)
	if rep.Daemon != "1.1.0" {
		t.Fatalf("daemon = %q, want 1.1.0", rep.Daemon)
	}
	if len(rep.Connectors) != 1 {
		t.Fatalf("connectors = %v, want deduped to one", rep.Connectors)
	}
	if rep.Verdict == "" {
		t.Fatal("verdict must always be set")
	}
	// Garbage in must not panic or invent a verdict.
	if got := versionGateFromHealth([]byte("not json")); got.Verdict != versionSevSkipped {
		t.Fatalf("unparseable health verdict = %q, want skipped", got.Verdict)
	}
}

// ── runVersionGate: refusal, warning, escape hatch ─────────────────────────

// healthBody builds a /health payload with the given daemon + connector version.
func healthBody(daemon, connector string) []byte {
	body := map[string]any{"service": "easyeda-agent", "version": daemon}
	if connector != "" {
		body["windows"] = []map[string]any{{"windowId": "w1", "connectorVersion": connector}}
	}
	b, _ := json.Marshal(body)
	return b
}

// withCLIVersion pins internal/version.Version (the ldflags-injected CLI
// version the gate compares against) for one test and restores it after.
func withCLIVersion(t *testing.T, v string) {
	t.Helper()
	prev := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = prev })
}

func TestRunVersionGateRefusesStaleDaemon(t *testing.T) {
	withCLIVersion(t, "v1.1.1")
	var stderr bytes.Buffer
	err := runVersionGate(&appConfig{}, healthBody("1.1.0", "1.1.1"), &stderr)
	if err == nil {
		t.Fatal("stale daemon must be refused")
	}
	msg := err.Error()
	for _, want := range []string{"拒绝执行", "daemon", "make dev", "easyeda daemon start", "--skip-version-check"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal message missing %q:\n%s", want, msg)
		}
	}
}

func TestRunVersionGateRefusesConnectorPatchDrift(t *testing.T) {
	withCLIVersion(t, "v1.1.1")
	var stderr bytes.Buffer
	err := runVersionGate(&appConfig{}, healthBody("1.1.1", "1.1.0"), &stderr)
	if err == nil {
		t.Fatal("connector patch drift must block in stability-first mode")
	}
	out := err.Error() + "\n" + stderr.String()
	if !strings.Contains(out, "connector") || !strings.Contains(out, "完全退出并重启 EasyEDA") || !strings.Contains(out, "拒绝执行") {
		t.Fatalf("expected an actionable connector refusal, got:\n%s", out)
	}
}

func TestRunVersionGateAlignedIsSilent(t *testing.T) {
	withCLIVersion(t, "v1.1.1")
	var stderr bytes.Buffer
	if err := runVersionGate(&appConfig{}, healthBody("1.1.1", "1.1.1"), &stderr); err != nil {
		t.Fatalf("aligned toolchain must pass: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("aligned toolchain must print nothing, got:\n%s", stderr.String())
	}
}

func TestRunVersionGateDevBuildNeverBlocks(t *testing.T) {
	withCLIVersion(t, "v1.1.1-19-ge9552d8-dirty")
	var stderr bytes.Buffer
	if err := runVersionGate(&appConfig{}, healthBody("1.0.0", "0.9.0"), &stderr); err != nil {
		t.Fatalf("dev-stamped CLI must never be blocked: %v", err)
	}
}

func TestRunVersionGateEscapeHatchIsAudited(t *testing.T) {
	withCLIVersion(t, "v1.1.1")
	dir := t.TempDir()
	t.Setenv("EASYEDA_AUDIT_DIR", dir)

	var stderr bytes.Buffer
	if err := runVersionGate(&appConfig{skipVersionCheck: true}, healthBody("1.1.0", "1.1.1"), &stderr); err != nil {
		t.Fatalf("--skip-version-check must let the run proceed: %v", err)
	}
	if !strings.Contains(stderr.String(), "--skip-version-check") {
		t.Fatalf("a bypass must still be loud on stderr, got:\n%s", stderr.String())
	}

	rows := readVersionGateAuditRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 audit row, got %d", len(rows))
	}
	if rows[0]["action"] != "cli.version_check.skip" {
		t.Fatalf("audit action = %v, want cli.version_check.skip", rows[0]["action"])
	}
	res, _ := rows[0]["result"].(map[string]any)
	if res == nil || res["daemon"] != "1.1.0" {
		t.Fatalf("audit row must record the mismatch, got %v", rows[0])
	}
}

func TestRunVersionGateEscapeHatchViaEnv(t *testing.T) {
	withCLIVersion(t, "v1.1.1")
	t.Setenv("EASYEDA_AUDIT_DIR", t.TempDir())
	t.Setenv(envSkipVersionCheck, "1")
	var stderr bytes.Buffer
	if err := runVersionGate(&appConfig{}, healthBody("1.0.0", "1.1.1"), &stderr); err != nil {
		t.Fatalf("env escape hatch must work: %v", err)
	}
}

func TestVersionCheckSkippedFalsyEnvValues(t *testing.T) {
	for _, v := range []string{"", "0", "false", "no"} {
		t.Setenv(envSkipVersionCheck, v)
		if versionCheckSkipped(&appConfig{}) {
			t.Fatalf("%s=%q must NOT disarm the gate", envSkipVersionCheck, v)
		}
	}
	for _, v := range []string{"1", "true", "yes"} {
		t.Setenv(envSkipVersionCheck, v)
		if !versionCheckSkipped(&appConfig{}) {
			t.Fatalf("%s=%q must disarm the gate", envSkipVersionCheck, v)
		}
	}
}

// readVersionGateAuditRows decodes every JSONL row under dir.
func readVersionGateAuditRows(t *testing.T, dir string) []map[string]any {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read audit dir: %v", err)
	}
	var rows []map[string]any
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("decode audit row %q: %v", line, err)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func TestVersionGateSummaryCoversEveryVerdict(t *testing.T) {
	for _, v := range []string{versionSevOK, versionSevWarn, versionSevBlock, versionSevSkipped} {
		if got := versionGateSummary(versionGateReport{CLI: "v1.1.1", Verdict: v}); got == "" {
			t.Fatalf("no summary line for verdict %q", v)
		}
	}
}
