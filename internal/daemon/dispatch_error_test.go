package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

func TestDispatchTimeoutMarksMutationUncertainAndNonRetryable(t *testing.T) {
	req := &protocol.Request{
		Action:         "schematic.component.place",
		Mutates:        true,
		WriteSensitive: true,
	}
	resp := errorResponse("req-1", "DISPATCH_FAILED", "connector did not respond", "deadline exceeded")

	annotateDispatchTimeout(req, &resp, context.DeadlineExceeded)

	if resp.Error == nil || resp.Error.Uncertain == nil || !*resp.Error.Uncertain {
		t.Fatalf("mutation timeout must be explicitly uncertain: %+v", resp.Error)
	}
	if resp.Error.Retryable == nil || *resp.Error.Retryable {
		t.Fatalf("mutation timeout must not be blindly retryable: %+v", resp.Error)
	}
}

func TestDispatchTimeoutTreatsMutatesAsWriteSensitiveFallback(t *testing.T) {
	// Older/internal request paths may carry the original mutation bit without
	// the newer writeSensitive superset.  Keep the retry decision fail-closed.
	req := &protocol.Request{Action: "schematic.component.place", Mutates: true}
	resp := errorResponse("req-fallback", "DISPATCH_FAILED", "connector did not respond", "deadline exceeded")

	annotateDispatchTimeout(req, &resp, context.DeadlineExceeded)

	if resp.Error == nil || resp.Error.Uncertain == nil || !*resp.Error.Uncertain {
		t.Fatalf("Mutates=true must keep a timeout uncertain when writeSensitive is absent: %+v", resp.Error)
	}
	if resp.Error.Retryable == nil || *resp.Error.Retryable {
		t.Fatalf("Mutates=true timeout must not be blindly retryable: %+v", resp.Error)
	}
}

func TestDispatchTimeoutMarksReadRetryableAndCertain(t *testing.T) {
	req := &protocol.Request{Action: "pcb.components.list"}
	resp := errorResponse("req-2", "DISPATCH_FAILED", "connector did not respond", "deadline exceeded")

	annotateDispatchTimeout(req, &resp, context.DeadlineExceeded)

	if resp.Error == nil || resp.Error.Uncertain == nil || *resp.Error.Uncertain {
		t.Fatalf("read timeout should be certain (no document mutation): %+v", resp.Error)
	}
	if resp.Error.Retryable == nil || !*resp.Error.Retryable {
		t.Fatalf("read timeout should be explicitly retryable: %+v", resp.Error)
	}
}

func TestDispatchTimeoutLeavesKnownTransportErrorsUnannotated(t *testing.T) {
	req := &protocol.Request{Action: "schematic.component.place", Mutates: true, WriteSensitive: true}
	resp := errorResponse("req-3", "DISPATCH_FAILED", "connector did not respond", "write failed")

	annotateDispatchTimeout(req, &resp, errors.New("socket write failed"))

	if resp.Error == nil {
		t.Fatal("expected structured error")
	}
	if resp.Error.Uncertain != nil || resp.Error.Retryable != nil {
		t.Fatalf("non-timeout transport failure must keep its original semantics: %+v", resp.Error)
	}
}

func TestDispatchTimeoutTreatsContextSwitchAsWriteSensitive(t *testing.T) {
	req := &protocol.Request{Action: "document.open", WriteSensitive: true}
	resp := errorResponse("req-4", "DISPATCH_FAILED", "connector did not respond", "deadline exceeded")

	annotateDispatchTimeout(req, &resp, context.Canceled)

	if resp.Error == nil || resp.Error.Uncertain == nil || !*resp.Error.Uncertain {
		t.Fatalf("late context switch must be uncertain: %+v", resp.Error)
	}
	if resp.Error.Retryable == nil || *resp.Error.Retryable {
		t.Fatalf("late context switch must not be blindly retried: %+v", resp.Error)
	}
}
