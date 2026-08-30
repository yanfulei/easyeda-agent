package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

func TestConnDispatchRejectsDuplicateRequestID(t *testing.T) {
	first := make(chan *protocol.Response, 1)
	c := &conn{pending: map[string]chan *protocol.Response{"same": first}}

	resp, err := c.dispatch(context.Background(), protocol.Request{
		Envelope: protocol.Envelope{ID: "same"},
	})
	if !errors.Is(err, errDuplicateRequestID) {
		t.Fatalf("duplicate dispatch error = %v, want errDuplicateRequestID", err)
	}
	if resp != nil {
		t.Fatalf("duplicate dispatch response = %+v, want nil", resp)
	}

	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if got := c.pending["same"]; got != first {
		t.Fatalf("duplicate dispatch replaced the original pending channel: got %p, want %p", got, first)
	}
}

func TestConnPendingCleanupKeepsAReplacementReservation(t *testing.T) {
	first := make(chan *protocol.Response, 1)
	replacement := make(chan *protocol.Response, 1)
	c := &conn{}

	if err := c.registerPending("same", first); err != nil {
		t.Fatalf("register first pending: %v", err)
	}
	// Model a recovery path replacing the reservation. The normal dispatch path
	// rejects this situation; this test pins the cleanup invariant so an older
	// defer can never delete a newer waiter's channel.
	c.pendingMu.Lock()
	c.pending["same"] = replacement
	c.pendingMu.Unlock()
	c.unregisterPending("same", first)

	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if got := c.pending["same"]; got != replacement {
		t.Fatalf("old cleanup removed replacement reservation: got %p, want %p", got, replacement)
	}
}
