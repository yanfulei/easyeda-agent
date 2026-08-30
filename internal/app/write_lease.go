package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

const writeLeaseControlTimeout = 10 * time.Second

// acquireManufacturingWriteLease is the CLI side of the daemon-local
// manufacturing admission gate. The target document may be inactive at this
// point; the daemon binds the lease to the selected window's live project UUID
// and records the immutable target document UUID supplied by discovery.
func acquireManufacturingWriteLease(
	cfg *appConfig,
	window string,
	documentUUID string,
	documentType string,
	ttl time.Duration,
) (*protocol.WriteLease, error) {
	if cfg == nil {
		return nil, fmt.Errorf("write lease: missing app config")
	}
	if strings.TrimSpace(window) == "" {
		return nil, fmt.Errorf("write lease: concrete windowId is required")
	}
	req := protocol.WriteLeaseRequest{
		Operation:    "acquire",
		ClientID:     cliClientID(),
		WindowID:     window,
		Project:      cfg.project,
		DocumentUUID: documentUUID,
		DocumentType: documentType,
	}
	if ttl > 0 {
		req.TTLms = ttl.Milliseconds()
	}
	resp, err := postWriteLeaseControl(cfg, req, true)
	if err != nil {
		return nil, err
	}
	if resp.Lease == nil {
		return nil, fmt.Errorf("write lease acquire returned no lease")
	}
	return resp.Lease, nil
}

// releaseManufacturingWriteLease is idempotent. It needs no connector/window:
// if EasyEDA exits during a failed release, cleanup still reaches the daemon
// instead of leaving every other writer blocked until TTL.
func releaseManufacturingWriteLease(cfg *appConfig, leaseID string) error {
	if cfg == nil {
		return fmt.Errorf("write lease release: missing app config")
	}
	_, err := postWriteLeaseControl(cfg, protocol.WriteLeaseRequest{
		Operation: "release",
		LeaseID:   leaseID,
		ClientID:  cliClientID(),
	}, false)
	return err
}

func postWriteLeaseControl(cfg *appConfig, body protocol.WriteLeaseRequest, enforceVersion bool) (*protocol.WriteLeaseResponse, error) {
	portStart, portEnd, err := cfg.portRange()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeLeaseControlTimeout)
	defer cancel()
	scan := scanHealth(ctx, hostPortOptions{host: cfg.host, portStart: portStart, portEnd: portEnd})
	if scan.Found == nil {
		return nil, fmt.Errorf("no easyeda-agent daemon found on %s:%s (start it with `easyeda daemon start`)", cfg.host, scan.Ports)
	}
	if enforceVersion {
		if err := checkVersionGate(cfg, scan.Found.Raw, io.Discard); err != nil {
			return nil, err
		}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode write lease request: %w", err)
	}
	url := fmt.Sprintf("http://%s:%d/writelease", cfg.host, scan.Found.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build write lease request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("write lease request: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	closeErr := httpResp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read write lease response: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close write lease response: %w", closeErr)
	}
	var parsed protocol.WriteLeaseResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode write lease response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 || !parsed.OK {
		if parsed.Error != nil {
			return &parsed, fmt.Errorf("%s: %s", parsed.Error.Code, parsed.Error.Message)
		}
		return &parsed, fmt.Errorf("write lease request failed with HTTP %d", httpResp.StatusCode)
	}
	return &parsed, nil
}
