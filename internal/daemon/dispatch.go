package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// dispatchTimeout bounds how long the daemon waits for a connector to answer a
// forwarded action. Heavy reads on real schematics (full netlist extraction,
// BOM generation, multi-page snapshot) routinely take 20-40s, so the cap is
// generous; the connector still keeps its own ping/pong liveness, and HTTP
// callers can layer their own shorter timeouts on top.
const dispatchTimeout = 60 * time.Second

// dispatchTimeoutBounds clamp a caller-supplied Request.TimeoutMs. The daemon
// answers slightly BEFORE the caller's own deadline (see requestTimeout) so the
// caller gets a structured DISPATCH_FAILED, not a raw HTTP timeout.
const (
	minDispatchTimeout = 3 * time.Second
	maxDispatchTimeout = 10 * time.Minute
	dispatchGrace      = 2 * time.Second
)

// requestTimeout resolves the connector-wait budget for one request: the
// caller's TimeoutMs minus a grace window, clamped to sane bounds; the daemon
// default when the caller sent none.
func requestTimeout(req *protocol.Request) time.Duration {
	if req.TimeoutMs <= 0 {
		return dispatchTimeout
	}
	d := time.Duration(req.TimeoutMs)*time.Millisecond - dispatchGrace
	if d < minDispatchTimeout {
		return minDispatchTimeout
	}
	if d > maxDispatchTimeout {
		return maxDispatchTimeout
	}
	return d
}

// nonReentrant lists actions the daemon refuses to run concurrently per window.
// A DRC re-check piles a second full-canvas recompute onto the webview while
// the first is still grinding (worst on a background window, where it NEVER
// finishes — optimization-loop.md A4); retries therefore make the hang worse,
// not better. The guard turns that into an immediate, explainable rejection.
//
// ── Does the connector's FIFO queue make this redundant? NO — keep it. ─────
//
// Connector 1.0.3 runs every action through one FIFO queue per window
// (extension/src/action-queue.ts), so two DRCs can no longer overlap on the
// webview. That removes the *overlap*, not the reason this guard exists:
//
//   - **Queuing and refusing are different answers.** A FIFO can only DELAY;
//     it has no way to decline. A second DRC would sit behind a 60s+ recompute
//     and then run a second full-canvas recompute on a webview that just
//     finished one — the caller waits minutes to learn what this guard says
//     immediately ("one is already running; if it never settles, foreground
//     the window"). The whole point is to stop the ask, not to sequence it.
//   - **Only the daemon can see the second asker.** The guard is what makes a
//     retry loop — or a second client/agent driving the same board — fail
//     loudly instead of silently stacking work.
//   - **A queued long action would burn its whole budget waiting.** With the
//     connector abandoning a head past its own timeoutMs, a DRC that queued
//     behind another DRC could be abandoned before it ever started, which is
//     both wasteful and reports as an ambiguous failure.
//
// And the converse: the daemon deliberately adds NO general per-window
// ordering beyond this. Serialization belongs at the connector, the only place
// that can serialize the *handlers* — a daemon-side queue would order the
// dispatches while the handlers still raced, i.e. cost without the guarantee.
var nonReentrant = map[string]bool{
	"pcb.drc.check":       true,
	"schematic.drc.check": true,
}

// knownActions is the set of Phase 1 action names the daemon will accept.
var knownActions = func() map[string]bool {
	set := map[string]bool{}
	for _, a := range protocol.AllActions() {
		set[a.Name] = true
	}
	return set
}()

// actionDomain maps each action to its domain, used to pick the right window
// when a project is open in several (a pcb.* action → the project's PCB window).
var actionDomain = func() map[string]protocol.Domain {
	m := map[string]protocol.Domain{}
	for _, a := range protocol.AllActions() {
		m[a.Name] = a.Domain
	}
	return m
}()

// docTypeForAction returns the documentType an action targets ("pcb" or
// "schematic"), matching the connector's documentType labels. Domain-agnostic
// actions (project/document/system/debug) return "" (no preference).
func docTypeForAction(action string) string {
	switch actionDomain[action] {
	case protocol.DomainPcb:
		return "pcb"
	case protocol.DomainSchematic:
		return "schematic"
	default:
		return ""
	}
}

// handleAction accepts a typed action request, validates it, answers daemon-local
// actions directly, and forwards the rest to the target connector.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req protocol.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, "BAD_REQUEST", "invalid action request body", err.Error()))
		return
	}

	// Assign a request id up front so every response, including errors, carries one.
	if req.ID == "" {
		req.ID = s.nextRequestID()
	}

	if req.Action == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, "ACTION_REQUIRED", "action is required", "include an \"action\" field"))
		return
	}
	if !knownActions[req.Action] {
		// Audit the rejection: a CLI↔daemon catalog drift (an action the CLI
		// ships but the daemon never registered) used to leave NO trace here,
		// so `audit-baseline` was blind to the exact failures that matter most.
		started := time.Now().UTC()
		errResp := errorResponse(req.ID, "UNKNOWN_ACTION", fmt.Sprintf("unknown action: %s", req.Action), "run `easyeda actions` for the supported set")
		s.audit.Append(fromResponse(started, &req, &errResp))
		writeJSON(w, http.StatusBadRequest, errResp)
		return
	}

	// system.health is answered by the daemon itself; it needs no connector.
	if req.Action == "system.health" {
		started := time.Now().UTC()
		resp := s.systemHealthResponse(req.ID)
		s.audit.Append(fromResponse(started, &req, &resp))
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Stable-identity routing: resolve a --project hint to a windowId. The
	// ephemeral windowId churns on reconnect, so callers can target a project
	// name/uuid and let the daemon find its current window.
	if req.WindowID == "" && req.Project != "" {
		id, found, ambiguous := s.hub.windowForProject(req.Project, docTypeForAction(req.Action))
		if ambiguous {
			started := time.Now().UTC()
			errResp := errorResponse(req.ID, "AMBIGUOUS_PROJECT", fmt.Sprintf("multiple connected windows match project %q", req.Project), "pass --window to pick one (see `easyeda health`)")
			s.audit.Append(fromResponse(started, &req, &errResp))
			writeJSON(w, http.StatusConflict, errResp)
			return
		}
		if !found {
			started := time.Now().UTC()
			errResp := errorResponse(req.ID, "NO_CONNECTOR", fmt.Sprintf("no connected window for project %q", req.Project), "open the project in EasyEDA (connector enabled), or run `easyeda health`")
			s.audit.Append(fromResponse(started, &req, &errResp))
			writeJSON(w, http.StatusServiceUnavailable, errResp)
			return
		}
		req.WindowID = id
	}

	// A windowId the caller is holding may have been retired by a plain page
	// refresh (the connector mints a fresh uuid on every handshake). Forward the
	// request to the window that replaced it rather than reporting a link
	// failure that did not happen — and tell the caller the new id so its next
	// call is direct.
	var redirectWarning string
	target, ok := s.hub.target(req.WindowID)
	if !ok && req.WindowID != "" {
		if newID, prev, resolved := s.hub.resolveRetired(req.WindowID); resolved {
			if c, found := s.hub.get(newID); found {
				via := "same project"
				if prev.DocumentUUID != "" {
					via = "same document"
				}
				redirectWarning = fmt.Sprintf(
					"window %s was retired (a page refresh mints a new windowId); re-routed to %s via %s. Use --project %q (stable) instead of --window.",
					req.WindowID, newID, via, prev.ProjectName)
				s.logf("re-routed stale window %s → %s (%s)", req.WindowID, newID, via)
				target, ok = c, true
				req.WindowID = newID
			}
		}
	}
	if !ok {
		started := time.Now().UTC()
		liveCount, liveSummary := s.hub.liveWindowSummary()
		// Distinguish "nothing is connected" from "your id is stale but the
		// connector is fine". Reporting the second as the first is what sends
		// agents off restarting a daemon that was never down.
		code, message, detail := "NO_CONNECTOR", "no EasyEDA connector is available", "start EasyEDA with the connector extension, then retry"
		switch {
		case req.WindowID != "" && liveCount > 0:
			code = "STALE_WINDOW"
			message = fmt.Sprintf("window %q is not connected, but %d connector window(s) ARE", req.WindowID, liveCount)
			detail = fmt.Sprintf("a page refresh mints a new windowId — the connector is fine. Connected now: %s. Route by --project <name> (stable across refreshes) instead of --window.", liveSummary)
		case req.WindowID != "":
			detail = fmt.Sprintf("no connector registered for window %q, and no window is connected at all — check `easyeda health`", req.WindowID)
		case liveCount > 1:
			code = "AMBIGUOUS_WINDOW"
			message = fmt.Sprintf("%d connector windows are connected; the target is ambiguous", liveCount)
			detail = fmt.Sprintf("pass --project <name> (preferred) or --window <id>. Connected now: %s", liveSummary)
		}
		errResp := errorResponse(req.ID, code, message, detail)
		s.audit.Append(fromResponse(started, &req, &errResp))
		writeJSON(w, http.StatusServiceUnavailable, errResp)
		return
	}

	req.Type = protocol.TypeRequest
	if req.Version == "" {
		req.Version = "v1"
	}
	req.CreatedAt = time.Now().UTC()
	req.WindowID = target.id()
	// Server-authoritative write classification for the connector quarantine.
	// Never trust a raw caller's JSON value: the catalog minus dry-run previews
	// is the same predicate autosave/stale-read gates already use.
	req.Mutates = requestMutates(&req)
	// Context switches stay non-mutating for autosave/stale-read/workflow logic,
	// but are write-sensitive for abandoned-handler quarantine and release leases.
	// Raw callers cannot forge either classification.
	req.WriteSensitive = requestWriteSensitive(&req)
	// Manufacturing write lease: admission and lease acquisition share one
	// mutex, so another client cannot slip a mutation through between "lease
	// acquired" and the first release action. Reads stay available for
	// diagnostics; document switches are treated like writes because they drift
	// the foreground target.
	if s.writeLeases != nil {
		releaseAdmission, held, leaseErr := s.writeLeases.admit(
			writeLeaseTargetFromWindow(target.snapshot()),
			req.ClientID,
			req.WriteSensitive,
		)
		if leaseErr != nil {
			started := time.Now().UTC()
			detail := "wait for manufacturing release to finish or for its TTL to expire; do not retry this mutation in a loop"
			if held != nil {
				detail = fmt.Sprintf("%s (leaseId=%s target=%s/%s)", detail, held.LeaseID, held.ProjectUUID, held.DocumentUUID)
			}
			errResp := errorResponse(req.ID, "WRITE_LEASE_HELD",
				"manufacturing release holds the project write lease; action was NOT dispatched",
				detail)
			uncertain, retryable := false, true
			errResp.Error.Uncertain = &uncertain
			errResp.Error.Retryable = &retryable
			s.audit.Append(fromResponse(started, &req, &errResp))
			writeJSON(w, http.StatusLocked, errResp)
			return
		}
		defer releaseAdmission()
	}
	// A same-version stale .eext is indistinguishable by semver but can expose a
	// different action/schema surface. Reads remain available for diagnosis;
	// writes fail closed before anything reaches the editor.
	if req.WriteSensitive {
		daemonFP := protocol.CurrentRuntimeFingerprints()
		connectorFP := target.runtimeFingerprints()
		missing := connectorFP.MissingFields()
		mismatches := protocol.FingerprintMismatches(daemonFP, connectorFP)
		if len(missing) > 0 || len(mismatches) > 0 {
			started := time.Now().UTC()
			problems := make([]string, 0, 2)
			if len(missing) > 0 {
				problems = append(problems, "missing fields: "+strings.Join(missing, ", "))
			}
			if len(mismatches) > 0 {
				problems = append(problems, "mismatched fields: "+strings.Join(mismatches, ", "))
			}
			errResp := errorResponse(req.ID, "RUNTIME_FINGERPRINT_MISMATCH",
				"daemon and EasyEDA connector runtime identity is incomplete or different; action was NOT dispatched",
				strings.Join(problems, "; ")+"; diagnostics/read actions remain available. Rebuild/re-import the connector and restart the daemon from the same source tree")
			uncertain, retryable := false, false
			errResp.Error.Uncertain = &uncertain
			errResp.Error.Retryable = &retryable
			s.audit.Append(fromResponse(started, &req, &errResp))
			writeJSON(w, http.StatusConflict, errResp)
			return
		}
	}

	// Workflow stage gate (issue #97): routing actions refuse until the
	// project's persisted stage state authorizes them — enforced HERE, at the
	// choke point, so a raw /action caller can't bypass the CLI's gates.
	if errResp := s.checkStageGate(&req); errResp != nil {
		started := time.Now().UTC()
		s.audit.Append(fromResponse(started, &req, errResp))
		writeJSON(w, http.StatusForbidden, *errResp)
		return
	}

	// Stale-read gate (SKILL 铁律 5, mechanical): a PCB read on a window mutated
	// since its last `doc reload` is REFUSED here, not merely annotated — the
	// advisory version was overridden 1780 times (18.1% of the reads it flagged)
	// over a 49-day audit window. 409 Conflict, not 403: the caller is not
	// unauthorized, its view of the document state conflicts with the engine's.
	// `--force-reason` still gets through, audited. See stalereads.go.
	if errResp := s.checkStaleRead(&req); errResp != nil {
		started := time.Now().UTC()
		s.audit.Append(fromResponse(started, &req, errResp))
		writeJSON(w, http.StatusConflict, *errResp)
		return
	}

	// Connector-FIFO gate (queueblock.go): when a light QUEUED read has been
	// unanswered for seconds while the BYPASS read still answers, the connector's
	// per-window action queue is proven stuck behind one wedged handler. Anything
	// sent now would just queue behind it and time out too (真机:12 条 pages.list
	// 各烧满 18s 才报「connector did not respond」,合计 216 秒). Refuse instantly,
	// name the head, and say "do NOT re-issue the write" — the stuck handler is
	// still running and its effect may land later.
	if errResp := s.checkQueueBlocked(&req); errResp != nil {
		started := time.Now().UTC()
		s.audit.Append(fromResponse(started, &req, errResp))
		writeJSON(w, http.StatusServiceUnavailable, *errResp)
		return
	}

	// Re-entrancy guard: refuse to stack a second DRC onto a window whose first
	// one hasn't settled — retrying piles recompute tasks onto the webview.
	if nonReentrant[req.Action] {
		release, acquired := s.acquireExclusive(req.Action, req.WindowID)
		if !acquired {
			started := time.Now().UTC()
			errResp := errorResponse(req.ID, "ACTION_BUSY",
				fmt.Sprintf("%s is already running on this window", req.Action),
				"wait for the in-flight check to settle; if it never does, EasyEDA is in the background — bring the window to the FOREGROUND and run once (do not retry in a loop)")
			s.audit.Append(fromResponse(started, &req, &errResp))
			writeJSON(w, http.StatusConflict, errResp)
			return
		}
		defer release()
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout(&req))
	defer cancel()

	started := time.Now().UTC()
	// Adaptive backoff (writehealth.go, REPORT round2 新 3): every forwarded
	// outcome feeds the rolling per-window health; whitelisted idempotent
	// actions get one light-read-gated retry; everything else passes failures
	// through (annotated with a degraded advisory below — never blind-resent).
	// The outcome carries the EFFECT verdict, not just the return code: a
	// response that re-read the canvas and reported survivors/notApplied counts
	// as a failed write however green its ok flag is (通道 A).
	hooks := adaptiveHooks{
		observe: func(o outcome) { s.writeHealth.observe(req.WindowID, o) },
		auditFirst: func(firstResp *protocol.Response, firstErr error) {
			first := firstResp
			if first == nil {
				er := errorResponse(req.ID, "DISPATCH_FAILED", "connector did not respond", firstErr.Error())
				first = &er
			}
			first.Warnings = append(first.Warnings, "superseded by daemon auto-retry (adaptive backoff)")
			s.audit.Append(fromResponse(started, &req, first))
			started = time.Now().UTC() // the final audit entry times the retry only
		},
		sleep: time.Sleep,
	}
	// Mark this window busy for the autosave gate: a debounced save must land in
	// an IDLE gap, never in the middle of a batch (autosave.go). Deferred rather
	// than released inline so no early return can leak the counter.
	defer s.beginClientAction(req.WindowID)()
	resp, err, _ := forwardWithAdaptiveRetry(ctx, req, target.dispatch, hooks)
	if err != nil {
		if errors.Is(err, errDuplicateRequestID) {
			// A caller reused an id that is still in flight. This is a caller
			// conflict, not a connector timeout: do not arm queue health or
			// adaptive retry state, and tell the caller to choose a fresh id.
			errResp := errorResponse(req.ID, "DUPLICATE_REQUEST_ID", "request id is already in flight", "use a unique request id or wait for the original request to finish")
			s.audit.Append(fromResponse(started, &req, &errResp))
			writeJSON(w, http.StatusConflict, errResp)
			return
		}
		// A timed-out mutation may still land after the caller has gone. Arm the
		// durability safety net now: save is idempotent, and the connector's write
		// quarantine will refuse/reschedule it until the original handler settles.
		// Without this, a late ghost write could remain only in memory forever.
		if req.Mutates && isTimeoutErr(err) {
			s.maybeAutosave(&req)
		}
		// One timed-out FIFO action is not yet proof of a blocked queue — a light
		// queued read that stays unanswered IS. Fire that probe now (never awaited)
		// so the NEXT command gets an instant answer instead of another full budget
		// of silence. See queueblock.go.
		s.armQueueProbe(target, &req, err)
		errResp := errorResponse(req.ID, "DISPATCH_FAILED", "connector did not respond", err.Error())
		// A timeout is different from a rejected request: the connector may
		// have started the handler and only lost the response.  Surface that
		// distinction in the protocol so an agent cannot blindly replay a
		// document mutation and create a duplicate.  Read-only calls remain
		// explicitly retryable; context switches are write-sensitive because a
		// late switch can still change the foreground target.
		annotateDispatchTimeout(&req, &errResp, err)
		s.writeHealth.annotateDegraded(&req, &errResp)
		s.audit.Append(fromResponse(started, &req, &errResp))
		writeJSON(w, http.StatusGatewayTimeout, errResp)
		return
	}
	// The connector echoes id/version/ok/result/context/artifacts but does not
	// stamp createdAt; the daemon owns the wall-clock for forwarded responses.
	if resp.CreatedAt.IsZero() {
		resp.CreatedAt = time.Now().UTC()
	}
	if resp.Type == "" {
		resp.Type = protocol.TypeResponse
	}
	// Surface the stale-id re-route on the successful response: the call worked,
	// but the caller's windowId is dead and its NEXT call should use the new one
	// (or --project). Silently succeeding would leave it holding a dead id.
	if redirectWarning != "" {
		resp.Warnings = append(resp.Warnings, redirectWarning)
	}
	s.persistArtifacts(resp, s.artifactDir(req.OutputDir))
	// Catalog-driven stage invalidation: a successful placement/outline mutation
	// clears stale downstream confirmations, whoever the client was.
	s.maybeInvalidateStage(&req, resp)
	// Stale-read state machine (SKILL iron rule 5): mark the window after a PCB
	// mutation, clear on reload/pour-rebuild. The REFUSAL happens before dispatch
	// (checkStaleRead above); what is left here is the staleRisk annotation for
	// the reads the gate let through — block-exempt (pcb.snapshot) and forced
	// ones. See stalereads.go.
	s.staleReads.observe(&req, resp)
	// Concurrent-writer advisory (issue #108): when a DIFFERENT client mutates
	// a window another client wrote to recently, annotate the response with a
	// non-blocking concurrentWriter field. See concurrentwrites.go.
	s.concurrentWrites.observe(&req, resp)
	// Degraded-connector advisory (REPORT round2 新 3): a FAILED response — or an
	// ok response the connector's own re-read proves did not fully land (假成功)
	// — on a window whose rolling health is degraded carries a structured hint;
	// mutating actions get the fake-failure-law advice (verify by light read
	// before resending). Nothing is retried here. See writehealth.go.
	s.writeHealth.annotateDegraded(&req, resp)
	s.audit.Append(fromResponse(started, &req, resp))
	// After a successful content-changing action, arm a debounced autosave so the
	// work reaches disk without the agent having to remember to save (no-op when
	// autosave is disabled or the action doesn't mutate). See autosave.go.
	if resp.OK {
		s.maybeAutosave(&req)
	}
	writeJSON(w, http.StatusOK, resp)
}

// artifactDir picks where to persist artifacts. The CLI sends its own working
// directory (outputDir) so files land in the user's project under a hidden
// .easyeda/artifacts dir — not the daemon's cwd. Callers that don't send one
// (tests, raw HTTP) fall back to the configured ArtifactDir, then "artifacts".
func (s *Server) artifactDir(outputDir string) string {
	if outputDir != "" {
		return filepath.Join(stripArtifactNesting(outputDir), ".easyeda", "artifacts")
	}
	if s.opts.ArtifactDir != "" {
		return s.opts.ArtifactDir
	}
	return "artifacts"
}

// stripArtifactNesting truncates a path to just BEFORE its first
// ".easyeda/artifacts" segment pair, making artifactDir's Join idempotent: an
// outputDir that already points INSIDE an artifact tree (a CLI whose cwd
// drifted there) used to grow .easyeda/artifacts/.easyeda/artifacts/… one
// level per call. A path without the pair is returned cleaned but otherwise
// unchanged. The CLI applies the same normalization before sending outputDir
// (internal/app dispatch.go) — this is the daemon-side defense for raw
// HTTP/older clients; keep both in sync.
func stripArtifactNesting(p string) string {
	clean := filepath.Clean(p)
	sep := string(filepath.Separator)
	segs := strings.Split(clean, sep)
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == ".easyeda" && segs[i+1] == "artifacts" {
			trimmed := strings.Join(segs[:i], sep)
			if trimmed == "" {
				if filepath.IsAbs(clean) {
					return sep
				}
				return "."
			}
			return trimmed
		}
	}
	return clean
}

// artifactFileName builds a sortable, findable filename: a local timestamp
// prefix (YYYYMMDD-HHMMSS) so files list in chronological order, plus the kind
// and a short id for findability and uniqueness within the same second.
//
//	e.g. 20260627-143022-schematic_snapshot-1a2b3c4d.png
func artifactFileName(a *protocol.Artifact, ts time.Time) string {
	parts := []string{ts.Local().Format("20060102-150405")}
	if a.Kind != "" {
		parts = append(parts, a.Kind)
	}
	short := strings.TrimPrefix(a.ID, "art_")
	if len(short) > 8 {
		short = short[:8]
	}
	if short != "" {
		parts = append(parts, short)
	}
	return strings.Join(parts, "-") + filepath.Ext(a.FileName)
}

// persistArtifacts writes any inline (base64) artifact bytes returned by the
// connector to dir, fills Path/Size/SHA256, and clears the inline bytes so they
// are not echoed back to the caller. Failures are reported as warnings rather
// than failing the whole action.
func (s *Server) persistArtifacts(resp *protocol.Response, dir string) {
	if resp == nil || len(resp.Artifacts) == 0 {
		return
	}

	for i := range resp.Artifacts {
		a := &resp.Artifacts[i]
		if a.InlineBase64 == "" {
			continue
		}

		data, err := base64.StdEncoding.DecodeString(a.InlineBase64)
		a.InlineBase64 = ""
		if err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("artifact %s: invalid base64: %v", a.ID, err))
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("artifact %s: create dir: %v", a.ID, err))
			continue
		}

		full := filepath.Join(dir, artifactFileName(a, resp.CreatedAt))
		if err := os.WriteFile(full, data, 0o644); err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("artifact %s: write: %v", a.ID, err))
			continue
		}

		if abs, err := filepath.Abs(full); err == nil {
			a.Path = abs
		} else {
			a.Path = full
		}
		sum := sha256.Sum256(data)
		a.Size = int64(len(data))
		a.SHA256 = hex.EncodeToString(sum[:])
	}

	// Mirror the absolute on-disk path(s) INTO result: agents read `result`
	// first and routinely miss the sibling top-level `artifacts` array, then
	// can't find the file (live feedback). result.artifactPath = first (the
	// common single-artifact case); artifactPaths lists all when there are more.
	paths := make([]string, 0, len(resp.Artifacts))
	for i := range resp.Artifacts {
		if p := resp.Artifacts[i].Path; p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 {
		if resp.Result == nil {
			resp.Result = map[string]any{}
		}
		resp.Result["artifactPath"] = paths[0]
		if len(paths) > 1 {
			resp.Result["artifactPaths"] = paths
		}
	}
}

// systemHealthResponse reports daemon liveness and the connected windows.
func (s *Server) systemHealthResponse(id string) protocol.Response {
	windows := s.hub.listAnnotated(s.opts.Version)
	ids := make([]string, 0, len(windows))
	for _, w := range windows {
		ids = append(ids, w.WindowID)
	}
	result := map[string]any{
		"service":      Service,
		"version":      s.opts.Version,
		"fingerprints": protocol.CurrentRuntimeFingerprints(),
		"windows":      windows,
		"windowIds":    ids,
	}
	if s.writeLeases != nil {
		if leases := s.writeLeases.list(); len(leases) > 0 {
			result["writeLeases"] = leases
		}
	}
	return protocol.Response{
		Envelope: protocol.Envelope{
			ID:        id,
			Type:      protocol.TypeResponse,
			Version:   "v1",
			CreatedAt: time.Now().UTC(),
		},
		OK:     true,
		Result: result,
	}
}

func (s *Server) nextRequestID() string {
	return fmt.Sprintf("req_%d", s.reqSeq.Add(1))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func errorResponse(id, code, message, detail string) protocol.Response {
	return protocol.Response{
		Envelope: protocol.Envelope{
			ID:        id,
			Type:      protocol.TypeResponse,
			Version:   "v1",
			CreatedAt: time.Now().UTC(),
		},
		OK: false,
		Error: &protocol.ErrorInfo{
			Code:    code,
			Message: message,
			Detail:  detail,
		},
	}
}

// annotateDispatchTimeout records the only safe retry semantics for a
// connector wait that expired at the daemon boundary. A handler may already
// be running in the editor even though no response reached us, so mutations
// and foreground context switches are uncertain and must be inspected before
// any replay. Pure reads have no document side effect and can be retried.
//
// Keep this helper separate from errorResponse: most daemon refusals are known
// zero-dispatch outcomes and should not inherit timeout uncertainty.
func annotateDispatchTimeout(req *protocol.Request, resp *protocol.Response, err error) {
	if req == nil || resp == nil || resp.Error == nil || !isTimeoutErr(err) {
		return
	}
	// Keep the mutation bit as a defensive fallback.  The daemon normally
	// stamps both fields before dispatch, but transitional callers/connectors
	// may only carry Mutates; a mutation must never become blindly retryable
	// merely because the newer superset field was absent.
	uncertain := req.WriteSensitive || req.Mutates
	retryable := !uncertain
	resp.Error.Uncertain = &uncertain
	resp.Error.Retryable = &retryable
}
