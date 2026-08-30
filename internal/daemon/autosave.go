package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// Daemon-level debounced autosave.
//
// `place`/`wire`/`modify` only mutate the in-memory EasyEDA document; without a
// save they never hit disk, so a window reload / daemon restart / EasyEDA crash
// silently loses the work (observed live: placed parts vanished after the daemon
// hot-reloaded). This is the infrastructure safety net — after any content-
// changing action the daemon arms a trailing-debounce timer and saves once the
// edits quiesce, so the agent doesn't have to remember to. Opt-in via
// Options.AutosaveDebounce (0 = off).

// mutatesAction maps each action name to whether it mutates the document, so the
// daemon fires an autosave after content-changing actions only.
var mutatesAction = func() map[string]bool {
	m := map[string]bool{}
	for _, a := range protocol.AllActions() {
		m[a.Name] = a.Mutates
	}
	return m
}()

// changesContextAction is catalog-owned for the same reason as mutatesAction:
// callers cannot downgrade a document switch to escape connector quarantine.
var changesContextAction = func() map[string]bool {
	m := map[string]bool{}
	for _, a := range protocol.AllActions() {
		m[a.Name] = a.ChangesContext
	}
	return m
}()

// supportsDryRunAction is catalog-owned. The payload is untrusted input; only
// handlers that explicitly declare a dry-run preview may use dryRun=true to
// become a read. An arbitrary dryRun=true field on a write remains a real
// mutation and keeps all write safety gates armed.
var supportsDryRunAction = func() map[string]bool {
	m := map[string]bool{}
	for _, a := range protocol.AllActions() {
		if a.SupportsDryRun {
			m[a.Name] = true
		}
	}
	return m
}()

// dryRunPayloadField is the payload key every catalog-declared dry-runnable
// action uses to mark a request as a PREVIEW. Other CLI --dry-run flags
// short-circuit before dispatch and therefore do not need a daemon request.
const dryRunPayloadField = "dryRun"

// isDryRunRequest reports whether a request is a preview that changes nothing.
// Strictly a JSON `true` — an unparseable/absent flag counts as a real write,
// which is the safe direction to err (a missed preview only costs a redundant
// save; a misread write would lose the safety net entirely).
func isDryRunRequest(req *protocol.Request) bool {
	if req == nil || !supportsDryRunAction[req.Action] {
		return false
	}
	v, ok := req.Payload[dryRunPayloadField]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// requestMutates reports whether a request actually changes the document: the
// catalog's Mutates flag minus previews explicitly supported by that same
// catalog. An arbitrary dryRun=true field remains a real write.
func requestMutates(req *protocol.Request) bool {
	return req != nil && mutatesAction[req.Action] && !isDryRunRequest(req)
}

// requestWriteSensitive is the connector-quarantine predicate. A context
// switch is not a document mutation, but it must wait for an abandoned write to
// settle or that late write can race onto the newly focused document.
func requestWriteSensitive(req *protocol.Request) bool {
	return requestMutates(req) || (req != nil && changesContextAction[req.Action])
}

// saveActionForDocType returns the typed save action for a documentType, or ""
// when none exists. Schematic and PCB both have a typed save; a PCB-mutating
// action therefore arms a debounced pcb.save the same way a schematic edit arms
// schematic.save.
func saveActionForDocType(docType string) string {
	switch docType {
	case "schematic":
		return "schematic.save"
	case "pcb":
		return "pcb.save"
	}
	return ""
}

// autosaveMaxDefer bounds how long a save may keep being pushed back because the
// window is busy (see deferAutosave). The safety net must stay a safety net: past
// this the save goes in regardless, accepting the head-of-line cost rather than
// letting a long busy stretch mean "never saved".
const autosaveMaxDefer = 60 * time.Second

// autosaver debounces per-window saves: a burst of edits on one window collapses
// into a single save fired `debounce` after the LAST edit (trailing debounce).
type autosaver struct {
	mu       sync.Mutex
	debounce time.Duration
	timers   map[string]*time.Timer
	// deferredSince records when a window's save first got pushed back because
	// the window was busy — the clock autosaveMaxDefer is measured against.
	deferredSince map[string]time.Time
	save          func(windowID, saveAction string)
}

func newAutosaver(debounce time.Duration, save func(windowID, saveAction string)) *autosaver {
	return &autosaver{
		debounce:      debounce,
		timers:        map[string]*time.Timer{},
		deferredSince: map[string]time.Time{},
		save:          save,
	}
}

// deferredFor reports how long this window's save has been pushed back, starting
// the clock on the first call. Cleared by clearDeferred once a save goes through.
func (a *autosaver) deferredFor(windowID string, now time.Time) time.Duration {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	since, ok := a.deferredSince[windowID]
	if !ok {
		a.deferredSince[windowID] = now
		return 0
	}
	return now.Sub(since)
}

func (a *autosaver) clearDeferred(windowID string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.deferredSince, windowID)
	a.mu.Unlock()
}

// schedule (re)arms the trailing-debounce timer for windowID. Each call resets
// the timer, so N rapid mutations coalesce into one save `debounce` after the
// last. nil/zero-debounce receiver is a no-op (autosave disabled).
func (a *autosaver) schedule(windowID, saveAction string) {
	if a == nil || a.debounce <= 0 || windowID == "" || saveAction == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.timers[windowID]; ok {
		t.Stop()
	}
	a.timers[windowID] = time.AfterFunc(a.debounce, func() {
		a.mu.Lock()
		delete(a.timers, windowID)
		a.mu.Unlock()
		a.save(windowID, saveAction)
	})
}

// stop cancels all pending timers (daemon shutdown). Pending edits are not force-
// flushed — flushing would race shutdown; the next session saves on first edit.
func (a *autosaver) stop() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.timers {
		t.Stop()
	}
	a.timers = map[string]*time.Timer{}
	a.deferredSince = map[string]time.Time{}
}

// maybeAutosave arms an autosave after a successful mutating action. It EXCLUDES
// the save action itself (schematic.save is Mutates=true) so a save never arms
// another save — no recursion.
func (s *Server) maybeAutosave(req *protocol.Request) {
	if s.autosave == nil || req == nil {
		return
	}
	if !requestMutates(req) {
		return
	}
	saveAction := saveActionForDocType(docTypeForAction(req.Action))
	if saveAction == "" || req.Action == saveAction {
		return
	}
	s.autosave.schedule(req.WindowID, saveAction)
}

// ── autosave 不得插进一批动作的中间(2026-08-24 真机定案) ───────────────────
//
// 连接器每窗口只有一条 FIFO 队列,而 `schematic.save` 在真板上是**长尾**动作:
// 今天 114 次保存 p50 只有 37ms,p95 却是 6.8s,最大 59.4s。autosave 又是一个
// 纯计时器:防抖到点就发,完全不看那个窗口此刻是不是正忙。结果就是它把一条
// 20-60 秒的阻塞塞进一批动作中间,后面所有 FIFO 动作(含 --doc 门那条 15ms 的
// schematic.pages.list)全部排在它后面超时。当天四次超时簇逐一对上:
//
//	03:14:43  connect_pin 18s 失败  ⟵ 叠在 schematic.save 35.8s 上
//	03:16:20  connect_pin 18s 失败  ⟵ 叠在 schematic.save 22.7s 上
//	03:16:48  connect_pin 18s 失败  ⟵ 叠在 schematic.save 44.2s 上
//	03:17:08  pages.list  18s 失败  ⟵ 同一条 44.2s 的 save
//	03:20:16  pages.list  18s 失败  ⟵ 叠在 schematic.save 59.4s 上
//
// 而且**保存是可以等的**:窗口还在被写,再等一会儿只会让这次保存覆盖更多内容。
// 所以这里只加一条判据 —— 窗口此刻有客户端动作在飞就重新防抖,把保存推到真正的
// 空档里去;上限 autosaveMaxDefer,保证它仍然是安全网而不是"永远不保存"。

// deferAutosave reports whether this save must be pushed back instead of sent
// now, re-arming the debounce when it is. True = caller must not dispatch.
func (s *Server) deferAutosave(windowID, saveAction string) bool {
	if s.autosave == nil {
		return false
	}
	// A manufacturing release owns every write in the project, including
	// daemon-originated saves. Do not apply autosaveMaxDefer here: forcing a save
	// after 60s would violate the lease exactly when a slow DRC/export needs it
	// most. Keep the timer armed; explicit release or TTL expiry resumes it.
	if s.writeLeases != nil && s.hub != nil {
		if target, ok := s.hub.target(windowID); ok {
			if held, _ := s.writeLeases.held(writeLeaseTargetFromWindow(target.snapshot())); held != nil {
				s.autosave.deferredFor(windowID, time.Now())
				s.autosave.schedule(windowID, saveAction)
				return true
			}
		}
	}
	busy := s.clientActionsInFlight(windowID) > 0
	if _, _, blocked := s.queueBlocks.blocked(windowID); blocked {
		// 队列已经堵死:再灌一条保存只会加长积压,而它照样得排队。
		busy = true
	}
	if !busy {
		s.autosave.clearDeferred(windowID)
		return false
	}
	if waited := s.autosave.deferredFor(windowID, time.Now()); waited >= autosaveMaxDefer {
		// 安全网优先:忙了太久也得落一次盘,哪怕要付队首阻塞的代价。
		s.autosave.clearDeferred(windowID)
		s.logf("autosave: %s on %s deferred %s (window busy) — saving anyway", saveAction, windowID, waited.Round(time.Second))
		return false
	}
	s.autosave.schedule(windowID, saveAction)
	return true
}

// dispatchSave forwards the debounced save to the window's connector. Best-effort
// and fired from a timer (no HTTP caller): logged + audited, never surfaced.
func (s *Server) dispatchSave(windowID, saveAction string) {
	target, ok := s.hub.target(windowID)
	if !ok {
		s.autosave.clearDeferred(windowID)
		return // window disconnected before the timer fired
	}
	if s.deferAutosave(windowID, saveAction) {
		return
	}
	// Close the final acquire-vs-autosave race. The lease check above and the
	// connector dispatch are not one atomic operation; reserving a normal
	// sensitive-action admission here means either this save is already in
	// flight (lease acquire returns WRITE_LEASE_BUSY) or the lease wins first
	// (this save is re-armed without touching EasyEDA).
	var releaseAdmission func()
	if s.writeLeases != nil {
		var leaseErr error
		releaseAdmission, _, leaseErr = s.writeLeases.admit(
			writeLeaseTargetFromWindow(target.snapshot()),
			"",
			true,
		)
		if leaseErr != nil {
			s.retryAutosave(windowID, saveAction)
			return
		}
		defer releaseAdmission()
	}
	req := protocol.Request{
		Envelope: protocol.Envelope{
			ID:        s.nextRequestID(),
			Type:      protocol.TypeRequest,
			Version:   "v1",
			WindowID:  windowID,
			CreatedAt: time.Now().UTC(),
		},
		Action:    saveAction,
		TimeoutMs: protocol.ActionTimeoutMs(saveAction),
		Mutates:   true,
	}
	ctx, cancel := context.WithTimeout(s.connCtx, requestTimeout(&req))
	defer cancel()
	started := time.Now().UTC()
	resp, err := target.dispatch(ctx, req)
	if err != nil {
		s.logf("autosave: %s on %s failed: %v", saveAction, windowID, err)
		s.retryAutosave(windowID, saveAction)
		return
	}
	s.audit.Append(fromResponse(started, &req, resp))
	s.logf("autosave: %s on %s (ok=%v)", saveAction, windowID, resp.OK)
	if !resp.OK && autosaveResponseRetryable(resp) {
		// Save is idempotent. A quarantine/queue refusal explicitly means it did
		// not execute; ACTION_ABANDONED is uncertain but re-saving after its
		// quarantine clears is still idempotent. Permanent SDK errors are not
		// hammered forever.
		s.retryAutosave(windowID, saveAction)
	}
}

func autosaveResponseRetryable(resp *protocol.Response) bool {
	if resp == nil || resp.Error == nil {
		return false
	}
	switch resp.Error.Code {
	case "MUTATION_QUARANTINED", "ACTION_EXPIRED", "QUEUE_OVERFLOW", "ACTION_ABANDONED":
		return true
	default:
		return false
	}
}

func (s *Server) retryAutosave(windowID, saveAction string) {
	if s.autosave == nil || windowID == "" || saveAction == "" {
		return
	}
	if s.connCtx != nil && s.connCtx.Err() != nil {
		return
	}
	s.autosave.schedule(windowID, saveAction)
}
