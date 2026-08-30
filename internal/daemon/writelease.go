package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

const (
	defaultWriteLeaseTTL = 15 * time.Minute
	minWriteLeaseTTL     = 30 * time.Second
	maxWriteLeaseTTL     = time.Hour
)

type writeLeaseTarget struct {
	WindowID     string
	ProjectUUID  string
	ProjectName  string
	DocumentUUID string
	DocumentType string
}

func writeLeaseTargetFromWindow(w Window) writeLeaseTarget {
	return writeLeaseTarget{
		WindowID:     strings.TrimSpace(w.WindowID),
		ProjectUUID:  strings.TrimSpace(w.Context.ProjectUUID),
		ProjectName:  strings.TrimSpace(w.Context.ProjectName),
		DocumentUUID: strings.TrimSpace(w.Context.DocumentUUID),
		DocumentType: strings.TrimSpace(w.Context.DocumentType),
	}
}

func (t writeLeaseTarget) key() string {
	if t.ProjectUUID == "" {
		return ""
	}
	return "project:" + t.ProjectUUID
}

func writeLeaseSensitive(req *protocol.Request) bool {
	return requestWriteSensitive(req)
}

type writeLeaseState struct {
	protocol.WriteLease
	ttl         time.Duration
	ownerActive int
}

// writeLeaseManager is a daemon-memory admission gate. Leases and active
// mutations share one mutex so acquire cannot race a write between "checked"
// and "forwarded".
type writeLeaseManager struct {
	mu     sync.Mutex
	now    func() time.Time
	next   uint64
	leases map[string]*writeLeaseState // project key -> lease
	byID   map[string]string           // lease id -> project key
	active map[string]int              // pre-lease mutating/context actions
}

func newWriteLeaseManager() *writeLeaseManager {
	return &writeLeaseManager{
		now:    time.Now,
		leases: map[string]*writeLeaseState{},
		byID:   map[string]string{},
		active: map[string]int{},
	}
}

func (m *writeLeaseManager) purgeExpiredLocked(key string, now time.Time) {
	lease := m.leases[key]
	if lease == nil || lease.ownerActive > 0 || now.Before(lease.ExpiresAt) {
		return
	}
	delete(m.byID, lease.LeaseID)
	delete(m.leases, key)
}

func (m *writeLeaseManager) acquire(
	target writeLeaseTarget,
	owner string,
	documentUUID string,
	documentType string,
	ttl time.Duration,
) (protocol.WriteLease, string, error) {
	owner = strings.TrimSpace(owner)
	documentUUID = strings.TrimSpace(documentUUID)
	documentType = strings.ToLower(strings.TrimSpace(documentType))
	if owner == "" {
		return protocol.WriteLease{}, "CLIENT_ID_REQUIRED", fmt.Errorf("clientId is required to acquire a manufacturing write lease")
	}
	key := target.key()
	if key == "" {
		return protocol.WriteLease{}, "WRITE_LEASE_UNBOUND", fmt.Errorf("target window has no projectUuid; refusing an unbound manufacturing lease")
	}
	if documentUUID == "" || documentType == "" {
		return protocol.WriteLease{}, "WRITE_LEASE_UNBOUND", fmt.Errorf("documentUuid and documentType are required for a manufacturing lease")
	}
	if ttl == 0 {
		ttl = defaultWriteLeaseTTL
	}
	if ttl < minWriteLeaseTTL || ttl > maxWriteLeaseTTL {
		return protocol.WriteLease{}, "WRITE_LEASE_TTL_INVALID", fmt.Errorf("lease TTL must be between %s and %s", minWriteLeaseTTL, maxWriteLeaseTTL)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	m.purgeExpiredLocked(key, now)
	if held := m.leases[key]; held != nil {
		if held.OwnerClientID != owner {
			return protocol.WriteLease{}, "WRITE_LEASE_HELD", fmt.Errorf(
				"project %s is already protected by lease %s until %s (owner %s)",
				target.ProjectUUID, held.LeaseID, held.ExpiresAt.Format(time.RFC3339), held.OwnerClientID)
		}
		if held.DocumentUUID != documentUUID || !strings.EqualFold(held.DocumentType, documentType) {
			return protocol.WriteLease{}, "WRITE_LEASE_TARGET_MISMATCH", fmt.Errorf(
				"client already holds lease %s for document %s (%s), not %s (%s)",
				held.LeaseID, held.DocumentUUID, held.DocumentType, documentUUID, documentType)
		}
		held.ExpiresAt = now.Add(ttl)
		held.ttl = ttl
		held.TTLms = ttl.Milliseconds()
		return held.WriteLease, "", nil
	}
	if m.active[key] > 0 {
		return protocol.WriteLease{}, "WRITE_LEASE_BUSY", fmt.Errorf(
			"project %s has %d mutating or context-changing action(s) in flight; wait for them to settle before manufacturing release",
			target.ProjectUUID, m.active[key])
	}

	m.next++
	id := fmt.Sprintf("wl_%d_%d", now.UnixMilli(), m.next)
	lease := &writeLeaseState{
		WriteLease: protocol.WriteLease{
			LeaseID:       id,
			OwnerClientID: owner,
			WindowID:      target.WindowID,
			ProjectUUID:   target.ProjectUUID,
			ProjectName:   target.ProjectName,
			DocumentUUID:  documentUUID,
			DocumentType:  documentType,
			AcquiredAt:    now,
			ExpiresAt:     now.Add(ttl),
			TTLms:         ttl.Milliseconds(),
		},
		ttl: ttl,
	}
	m.leases[key] = lease
	m.byID[id] = key
	return lease.WriteLease, "", nil
}

// admit atomically checks the lease and reserves an in-flight slot. All owner
// actions renew and hold the lease for their full dispatch duration. Without a
// lease, only writes/context switches reserve a slot, preventing acquire racing
// a write that was already admitted.
func (m *writeLeaseManager) admit(target writeLeaseTarget, clientID string, sensitive bool) (func(), *protocol.WriteLease, error) {
	key := target.key()
	if key == "" {
		return func() {}, nil, nil
	}
	clientID = strings.TrimSpace(clientID)
	m.mu.Lock()
	now := m.now().UTC()
	m.purgeExpiredLocked(key, now)
	held := m.leases[key]
	ownerAdmission := false
	if held != nil {
		if held.OwnerClientID != clientID {
			if sensitive {
				copy := held.WriteLease
				m.mu.Unlock()
				return nil, &copy, fmt.Errorf(
					"project %s is locked for manufacturing release by %s until %s",
					held.ProjectUUID, held.OwnerClientID, held.ExpiresAt.Format(time.RFC3339))
			}
		} else {
			held.ExpiresAt = now.Add(held.ttl)
			held.ownerActive++
			ownerAdmission = true
		}
	}
	if sensitive && held == nil {
		m.active[key]++
	}
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if ownerAdmission {
				if current := m.leases[key]; current != nil && current.OwnerClientID == clientID && current.ownerActive > 0 {
					current.ownerActive--
				}
				m.purgeExpiredLocked(key, m.now().UTC())
			} else if sensitive && m.active[key] > 0 {
				m.active[key]--
				if m.active[key] == 0 {
					delete(m.active, key)
				}
			}
		})
	}, nil, nil
}

func (m *writeLeaseManager) release(leaseID, owner string) (protocol.WriteLease, bool, string, error) {
	leaseID = strings.TrimSpace(leaseID)
	owner = strings.TrimSpace(owner)
	if leaseID == "" {
		return protocol.WriteLease{}, false, "WRITE_LEASE_ID_REQUIRED", fmt.Errorf("leaseId is required")
	}
	if owner == "" {
		return protocol.WriteLease{}, false, "CLIENT_ID_REQUIRED", fmt.Errorf("clientId is required to release a manufacturing write lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.byID[leaseID]
	if !ok {
		// Cleanup is deliberately idempotent: a command may finish after its TTL.
		return protocol.WriteLease{}, false, "", nil
	}
	m.purgeExpiredLocked(key, m.now().UTC())
	held := m.leases[key]
	if held == nil {
		return protocol.WriteLease{}, false, "", nil
	}
	if held.OwnerClientID != owner {
		return protocol.WriteLease{}, false, "WRITE_LEASE_OWNER_MISMATCH", fmt.Errorf("lease %s belongs to %s, not %s", leaseID, held.OwnerClientID, owner)
	}
	if held.ownerActive > 0 {
		return protocol.WriteLease{}, false, "WRITE_LEASE_BUSY", fmt.Errorf("lease %s still has %d owner action(s) in flight", leaseID, held.ownerActive)
	}
	copy := held.WriteLease
	delete(m.byID, leaseID)
	delete(m.leases, key)
	return copy, true, "", nil
}

func (m *writeLeaseManager) held(target writeLeaseTarget) (*protocol.WriteLease, time.Duration) {
	key := target.key()
	if key == "" {
		return nil, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	m.purgeExpiredLocked(key, now)
	held := m.leases[key]
	if held == nil {
		return nil, 0
	}
	copy := held.WriteLease
	remaining := held.ExpiresAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	return &copy, remaining
}

func (m *writeLeaseManager) list() []protocol.WriteLease {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	for key := range m.leases {
		m.purgeExpiredLocked(key, now)
	}
	out := make([]protocol.WriteLease, 0, len(m.leases))
	for _, held := range m.leases {
		out = append(out, held.WriteLease)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LeaseID < out[j].LeaseID })
	return out
}

func writeLeaseError(code string, err error) protocol.WriteLeaseResponse {
	return protocol.WriteLeaseResponse{
		OK: false,
		Error: &protocol.ErrorInfo{
			Code:    code,
			Message: err.Error(),
		},
	}
}

func writeLeaseDuration(ttlMs int64) (time.Duration, error) {
	if ttlMs == 0 {
		return 0, nil
	}
	if ttlMs < minWriteLeaseTTL.Milliseconds() || ttlMs > maxWriteLeaseTTL.Milliseconds() {
		return 0, fmt.Errorf("lease TTL must be between %s and %s", minWriteLeaseTTL, maxWriteLeaseTTL)
	}
	return time.Duration(ttlMs) * time.Millisecond, nil
}

func (s *Server) handleWriteLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req protocol.WriteLeaseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, writeLeaseError("BAD_REQUEST", fmt.Errorf("invalid write lease request: %w", err)))
		return
	}
	switch strings.ToLower(strings.TrimSpace(req.Operation)) {
	case "acquire":
		s.handleWriteLeaseAcquire(w, req)
	case "release":
		lease, released, code, err := s.writeLeases.release(req.LeaseID, req.ClientID)
		if err != nil {
			writeJSON(w, http.StatusConflict, writeLeaseError(code, err))
			return
		}
		resp := protocol.WriteLeaseResponse{OK: true, Released: released}
		if released {
			resp.Lease = &lease
			s.logf("write lease released id=%s project=%s document=%s owner=%s", lease.LeaseID, lease.ProjectUUID, lease.DocumentUUID, lease.OwnerClientID)
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		writeJSON(w, http.StatusBadRequest, writeLeaseError("WRITE_LEASE_OPERATION_INVALID", fmt.Errorf("operation must be acquire or release")))
	}
}

func (s *Server) handleWriteLeaseAcquire(w http.ResponseWriter, req protocol.WriteLeaseRequest) {
	var target *conn
	var ok bool
	if req.WindowID != "" {
		target, ok = s.hub.target(req.WindowID)
	} else if req.Project != "" {
		id, found, ambiguous := s.hub.windowForProject(req.Project, strings.ToLower(strings.TrimSpace(req.DocumentType)))
		if ambiguous {
			writeJSON(w, http.StatusConflict, writeLeaseError("AMBIGUOUS_PROJECT", fmt.Errorf("multiple connected windows match project %q; pass windowId", req.Project)))
			return
		}
		if found {
			target, ok = s.hub.target(id)
		}
	}
	if !ok || target == nil {
		writeJSON(w, http.StatusServiceUnavailable, writeLeaseError("NO_CONNECTOR", fmt.Errorf("no connected EasyEDA window matches the requested manufacturing target")))
		return
	}
	window := target.snapshot()
	bound := writeLeaseTargetFromWindow(window)
	selector := strings.TrimSpace(req.Project)
	if selector != "" && selector != bound.ProjectUUID && selector != bound.ProjectName {
		writeJSON(w, http.StatusConflict, writeLeaseError("WRITE_LEASE_PROJECT_MISMATCH", fmt.Errorf(
			"requested project %q does not match window project %q (%s)", selector, bound.ProjectName, bound.ProjectUUID)))
		return
	}
	ttl, ttlErr := writeLeaseDuration(req.TTLms)
	if ttlErr != nil {
		writeJSON(w, http.StatusBadRequest, writeLeaseError("WRITE_LEASE_TTL_INVALID", ttlErr))
		return
	}
	lease, code, err := s.writeLeases.acquire(
		bound,
		req.ClientID,
		req.DocumentUUID,
		req.DocumentType,
		ttl,
	)
	if err != nil {
		status := http.StatusConflict
		if code == "CLIENT_ID_REQUIRED" || code == "WRITE_LEASE_UNBOUND" || code == "WRITE_LEASE_TTL_INVALID" {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, writeLeaseError(code, err))
		return
	}
	s.logf("write lease acquired id=%s project=%s document=%s owner=%s expires=%s", lease.LeaseID, lease.ProjectUUID, lease.DocumentUUID, lease.OwnerClientID, lease.ExpiresAt.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, protocol.WriteLeaseResponse{OK: true, Lease: &lease})
}
