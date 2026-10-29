package backend

import (
	"context"
	"fmt"
	log "log/slog"
	"net/http"
)

type httpBackend struct {
	generic
	client *http.Client
}

func newHTTPBackend(config *Config) (*httpBackend, error) {
	if config.Client == nil {
		return nil, fmt.Errorf("failed to create HTTTP backend: nil client")
	}
	return &httpBackend{
		generic: generic{
			backendType:    HTTP,
			addr:           config.Address,
			port:           config.Port,
			family:         addressFamily(config.Address),
			kubeConfigPath: config.KubeConfigPath,
			isLocal:        config.IsLocal,
		},
		client: config.Client,
	}, nil
}

func (h *httpBackend) Check(ctx context.Context) bool {
	addr := h.addr
	if h.port != 0 {
		addr = fmt.Sprintf("%s:%d", h.addr, h.port)
	}
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if reqErr != nil {
		log.Error("create health check request", "err", reqErr)
		return false
	}
	resp, err := h.client.Do(req)
	if err != nil {
		log.Error("health check request failed", "url", addr, "err", err)
		return false
	}

	resp.Body.Close()
	healthy := resp.StatusCode == http.StatusOK
	if !healthy {
		log.Warn("health check returned non-200 status", "url", addr, "status", resp.StatusCode)
		return false
	}
	return true

}
