package daemon

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

func testLeaseTarget(project, window, document string) writeLeaseTarget {
	return writeLeaseTarget{
		WindowID:     window,
		ProjectUUID:  project,
		ProjectName:  "probe",
		DocumentUUID: document,
		DocumentType: "pcb",
	}
}

func TestWriteLeaseBlocksOtherWriterAcrossProjectWindows(t *testing.T) {
	m := newWriteLeaseManager()
	now := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	target := testLeaseTarget("project-1", "window-a", "pcb-1")

	lease, code, err := m.acquire(target, "host:10:release", "pcb-1", "pcb", 0)
	if err != nil {
		t.Fatalf("acquire: code=%s err=%v", code, err)
	}
	if lease.TTLms != defaultWriteLeaseTTL.Milliseconds() {
		t.Fatalf("default ttl = %d, want %d", lease.TTLms, defaultWriteLeaseTTL.Milliseconds())
	}

	// Reads from another client remain available for diagnosis.
	readDone, held, err := m.admit(target, "host:20:observer", false)
	if err != nil || held != nil {
		t.Fatalf("read should pass under lease, held=%+v err=%v", held, err)
	}
	readDone()

	// A different window showing the same project is still the same release
	// target. Locking only windowId would let a schematic edit invalidate BOM.
	otherWindow := testLeaseTarget("project-1", "window-b", "schematic-2")
	writeDone, held, err := m.admit(otherWindow, "host:20:observer", true)
	if err == nil || held == nil {
		if writeDone != nil {
			writeDone()
		}
		t.Fatalf("other writer on same project must be blocked, held=%+v err=%v", held, err)
	}
	if held.LeaseID != lease.LeaseID {
		t.Fatalf("blocked by lease %q, want %q", held.LeaseID, lease.LeaseID)
	}

	// A different project is independent.
	otherProjectDone, held, err := m.admit(testLeaseTarget("project-2", "window-c", "pcb-2"), "host:20:observer", true)
	if err != nil || held != nil {
		t.Fatalf("other project should pass, held=%+v err=%v", held, err)
	}
	otherProjectDone()
}

func TestWriteLeaseOwnerActionSurvivesTTLAndThenExpires(t *testing.T) {
	m := newWriteLeaseManager()
	now := time.Date(2026, 8, 30, 2, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	target := testLeaseTarget("project-1", "window-a", "pcb-1")
	lease, _, err := m.acquire(target, "owner", "pcb-1", "pcb", minWriteLeaseTTL)
	if err != nil {
		t.Fatal(err)
	}

	ownerDone, _, err := m.admit(target, "owner", false)
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	now = now.Add(2 * minWriteLeaseTTL)
	// TTL must never unlock the board while an owner's long DRC/export call is
	// still in flight.
	if done, held, blockErr := m.admit(target, "other", true); blockErr == nil || held == nil {
		if done != nil {
			done()
		}
		t.Fatalf("expired-but-active owner lease must still block, held=%+v err=%v", held, blockErr)
	}
	ownerDone()

	if held, _ := m.held(target); held != nil {
		t.Fatalf("lease should expire after owner action settles: %+v", held)
	}
	otherDone, held, err := m.admit(target, "other", true)
	if err != nil || held != nil {
		t.Fatalf("writer should pass after TTL expiry, held=%+v err=%v", held, err)
	}
	otherDone()
	if _, released, _, err := m.release(lease.LeaseID, "owner"); err != nil || released {
		t.Fatalf("expired release must be idempotent, released=%v err=%v", released, err)
	}
}

func TestWriteLeaseAcquireCannotRaceAdmittedMutation(t *testing.T) {
	m := newWriteLeaseManager()
	target := testLeaseTarget("project-1", "window-a", "pcb-1")

	mutationDone, _, err := m.admit(target, "writer", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, code, err := m.acquire(target, "release", "pcb-1", "pcb", 0); err == nil || code != "WRITE_LEASE_BUSY" {
		t.Fatalf("acquire during admitted mutation = code %q err %v", code, err)
	}
	mutationDone()
	if _, code, err := m.acquire(target, "release", "pcb-1", "pcb", 0); err != nil {
		t.Fatalf("acquire after mutation settles: code=%s err=%v", code, err)
	}
}

func TestWriteLeaseReleaseRequiresOwnerButIsOtherwiseIdempotent(t *testing.T) {
	m := newWriteLeaseManager()
	target := testLeaseTarget("project-1", "window-a", "pcb-1")
	lease, _, err := m.acquire(target, "owner", "pcb-1", "pcb", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, code, err := m.release(lease.LeaseID, "other"); err == nil || code != "WRITE_LEASE_OWNER_MISMATCH" {
		t.Fatalf("non-owner release = code %q err %v", code, err)
	}
	if _, released, code, err := m.release(lease.LeaseID, "owner"); err != nil || !released || code != "" {
		t.Fatalf("owner release = released %v code %q err %v", released, code, err)
	}
	if _, released, code, err := m.release(lease.LeaseID, "owner"); err != nil || released || code != "" {
		t.Fatalf("second release = released %v code %q err %v", released, code, err)
	}
}

func TestWriteLeaseHTTPAcquireAndActionGate(t *testing.T) {
	s := New(Options{})
	s.hub.add(connWithDoc("window-1", "project-1", "probe", "pcb-1", "pcb"))

	acquireBody := `{
		"operation":"acquire",
		"clientId":"owner",
		"windowId":"window-1",
		"project":"probe",
		"documentUuid":"pcb-1",
		"documentType":"pcb"
	}`
	rec := httptest.NewRecorder()
	s.handleWriteLease(rec, httptest.NewRequest(http.MethodPost, "/writelease", strings.NewReader(acquireBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("acquire HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var acquired protocol.WriteLeaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &acquired); err != nil || !acquired.OK || acquired.Lease == nil {
		t.Fatalf("bad acquire response: err=%v body=%s", err, rec.Body.String())
	}

	status, blocked := postActionTo(t, s, `{
		"action":"pcb.component.modify",
		"windowId":"window-1",
		"clientId":"other",
		"payload":{"primitiveId":"x","x":1}
	}`)
	if status != http.StatusLocked || blocked.Error == nil || blocked.Error.Code != "WRITE_LEASE_HELD" {
		t.Fatalf("mutation under lease: HTTP=%d response=%+v", status, blocked)
	}
	if blocked.Error.Uncertain == nil || *blocked.Error.Uncertain {
		t.Fatalf("blocked action must be certainly not dispatched: %+v", blocked.Error)
	}

	releaseBody := `{"operation":"release","clientId":"owner","leaseId":"` + acquired.Lease.LeaseID + `"}`
	rec = httptest.NewRecorder()
	s.handleWriteLease(rec, httptest.NewRequest(http.MethodPost, "/writelease", strings.NewReader(releaseBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("release HTTP %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAutosaveNeverForcesThroughWriteLease(t *testing.T) {
	s := New(Options{AutosaveDebounce: time.Hour})
	defer s.autosave.stop()
	c := connWithDoc("window-1", "project-1", "probe", "pcb-1", "pcb")
	s.hub.add(c)
	target := writeLeaseTargetFromWindow(c.snapshot())
	lease, _, err := s.writeLeases.acquire(target, "owner", "pcb-1", "pcb", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Even a save already deferred beyond autosaveMaxDefer must remain paused;
	// the old busy-window rule would force it through at this point.
	s.autosave.deferredSince["window-1"] = time.Now().Add(-2 * autosaveMaxDefer)
	if !s.deferAutosave("window-1", "pcb.save") {
		t.Fatal("autosave must be deferred while manufacturing lease is active")
	}
	if _, released, _, err := s.writeLeases.release(lease.LeaseID, "owner"); err != nil || !released {
		t.Fatalf("release: released=%v err=%v", released, err)
	}
	if s.deferAutosave("window-1", "pcb.save") {
		t.Fatal("autosave should resume once the lease is released")
	}
}

func TestWriteLeaseTreatsDocumentSwitchAsExclusive(t *testing.T) {
	for _, action := range []string{"document.open", "document.close", "schematic.page.open"} {
		req := &protocol.Request{Action: action}
		if !writeLeaseSensitive(req) {
			t.Errorf("%s must be lease-exclusive despite catalog Mutates=false", action)
		}
	}
	if writeLeaseSensitive(&protocol.Request{Action: "document.current"}) {
		t.Fatal("document.current is a diagnostic read and must remain available")
	}
}

func TestWriteLeaseDurationRejectsOverflowBeforeConversion(t *testing.T) {
	if _, err := writeLeaseDuration(math.MaxInt64); err == nil {
		t.Fatal("oversized ttlMs must be rejected before time.Duration conversion")
	}
	if got, err := writeLeaseDuration(0); err != nil || got != 0 {
		t.Fatalf("zero should select daemon default, got=%s err=%v", got, err)
	}
}
