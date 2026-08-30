package protocol

import "time"

// WriteLeaseRequest is the daemon-local control-plane request used by the
// manufacturing release workflow. It intentionally does not travel over the
// connector WebSocket: the lease protects connector dispatch itself.
type WriteLeaseRequest struct {
	Operation    string `json:"operation"`
	LeaseID      string `json:"leaseId,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	WindowID     string `json:"windowId,omitempty"`
	Project      string `json:"project,omitempty"`
	DocumentUUID string `json:"documentUuid,omitempty"`
	DocumentType string `json:"documentType,omitempty"`
	TTLms        int64  `json:"ttlMs,omitempty"`
}

// WriteLease identifies one project-scoped exclusive writer. Document and
// window fields preserve the exact manufacturing target for diagnostics, while
// ProjectUUID is the actual lock scope: a schematic edit in another window of
// the same project can invalidate a PCB release just as surely as a PCB edit.
type WriteLease struct {
	LeaseID       string    `json:"leaseId"`
	OwnerClientID string    `json:"ownerClientId"`
	WindowID      string    `json:"windowId"`
	ProjectUUID   string    `json:"projectUuid"`
	ProjectName   string    `json:"projectName,omitempty"`
	DocumentUUID  string    `json:"documentUuid"`
	DocumentType  string    `json:"documentType"`
	AcquiredAt    time.Time `json:"acquiredAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	TTLms         int64     `json:"ttlMs"`
}

type WriteLeaseResponse struct {
	OK       bool        `json:"ok"`
	Lease    *WriteLease `json:"lease,omitempty"`
	Released bool        `json:"released,omitempty"`
	Error    *ErrorInfo  `json:"error,omitempty"`
}
