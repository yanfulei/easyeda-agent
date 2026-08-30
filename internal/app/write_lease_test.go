package app

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

func TestManufacturingWriteLeaseClientRoundTrip(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []protocol.WriteLeaseRequest
		port     int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": "easyeda-agent",
			"version": "dev",
			"status":  "ok",
			"port":    port,
			"windows": []any{},
		})
	})
	mux.HandleFunc("/writelease", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.WriteLeaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		if req.Operation == "acquire" {
			_ = json.NewEncoder(w).Encode(protocol.WriteLeaseResponse{
				OK: true,
				Lease: &protocol.WriteLease{
					LeaseID:       "wl-test",
					OwnerClientID: req.ClientID,
					WindowID:      req.WindowID,
					ProjectUUID:   "project-uuid",
					ProjectName:   req.Project,
					DocumentUUID:  req.DocumentUUID,
					DocumentType:  req.DocumentType,
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.WriteLeaseResponse{OK: true, Released: true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	hostPort := strings.TrimPrefix(server.URL, "http://")
	host, portText, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatal(err)
	}
	port, err = strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &appConfig{
		host:             host,
		ports:            fmt.Sprintf("%d-%d", port, port),
		project:          "probe",
		skipVersionCheck: true,
	}
	lease, err := acquireManufacturingWriteLease(cfg, "window-1", "pcb-1", "pcb", 0)
	if err != nil {
		t.Fatalf("acquire client: %v", err)
	}
	if lease.LeaseID != "wl-test" || lease.DocumentUUID != "pcb-1" {
		t.Fatalf("unexpected lease: %+v", lease)
	}
	if err := releaseManufacturingWriteLease(cfg, lease.LeaseID); err != nil {
		t.Fatalf("release client: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want acquire + release", len(requests))
	}
	if requests[0].Operation != "acquire" || requests[0].WindowID != "window-1" ||
		requests[0].Project != "probe" || requests[0].DocumentUUID != "pcb-1" ||
		requests[0].ClientID == "" {
		t.Fatalf("bad acquire request: %+v", requests[0])
	}
	if requests[1].Operation != "release" || requests[1].LeaseID != "wl-test" ||
		requests[1].ClientID != requests[0].ClientID || requests[1].WindowID != "" {
		t.Fatalf("bad release request: %+v", requests[1])
	}
}

func TestWriteLeaseClientPreservesStructuredDaemonError(t *testing.T) {
	var port int
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": "easyeda-agent", "version": "dev", "status": "ok", "port": port, "windows": []any{},
		})
	})
	mux.HandleFunc("/writelease", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusLocked)
		_ = json.NewEncoder(w).Encode(protocol.WriteLeaseResponse{
			OK: false,
			Error: &protocol.ErrorInfo{
				Code:    "WRITE_LEASE_HELD",
				Message: "another release owns the project",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	host, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ = strconv.Atoi(portText)
	cfg := &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port), skipVersionCheck: true}

	_, err = acquireManufacturingWriteLease(cfg, "window-1", "pcb-1", "pcb", 0)
	if err == nil || !strings.Contains(err.Error(), "WRITE_LEASE_HELD") {
		t.Fatalf("structured lease error lost: %v", err)
	}
}
