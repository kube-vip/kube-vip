package backend

import (
	"context"
	"fmt"
	"net/http"
)

type httpBackend struct {
	generic
	client *http.Client
}

func newHTTPBackend(config *Config) (*httpBackend, error) {
	if config.Client == nil {
		return nil, fmt.Errorf("failed to create HTTP backend: nil client")
	}
	return &httpBackend{
		backendType: HTTP,
		addr:        config.Address,
		port:        config.Port,
		client:      config.Client,
	}, nil
}

func (h *httpBackend) Check(ctx context.Context) (bool, error) {
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, h.addr, nil)
	if reqErr != nil {
		return false, fmt.Errorf("failed to create health check request: %w", reqErr)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("health check request failed: %w", err)
	}

	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, h.addr)
	}
	return true, nil
}
