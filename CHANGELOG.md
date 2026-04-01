# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- Retry on 403 Forbidden and 401 Unauthorized in `ServicesWatcher` at startup with exponential backoff. Fixes #1464.
- Reintroduce BGP config via node annotations. Fixes #1488.
- Fail fast in runtime `manager` and `service` paths when legacy `vip_address` is used without `vip_subnet` in control-plane ARP, BGP, or Routing Table mode.
- Cancel the mode context on init or configuration failure before waiting on goroutines during shutdown.


### Added
- Configurable control-plane health check for BGP mode without leader election
  - Polls a configurable HTTP(S) endpoint (e.g. `https://localhost:6443/livez`) to verify the exposed service is healthy (usually the local kube-apiserver)
  - Withdraws the BGP route after a configurable number of consecutive failures, removing the unhealthy node from the ECMP set
  - Re-announces the route automatically once the endpoint recovers
  - Gracefully withdraws the route on shutdown (SIGTERM)
  - Supports custom CA certificates for TLS verification
  - Configuration via environment variables or CLI flags:
    - `control_plane_health_check_address` / `--controlPlaneHealthCheckAddress`: URL to poll
    - `control_plane_health_check_period_seconds` / `--controlPlaneHealthCheckPeriodSeconds`: interval between checks (default: 5)
    - `control_plane_health_check_timeout_seconds` / `--controlPlaneHealthCheckTimeoutSeconds`: per-request timeout (default: 3)
    - `control_plane_health_check_failure_threshold` / `--controlPlaneHealthCheckFailureThreshold`: consecutive failures before withdrawal (default: 3)
    - `control_plane_health_check_ca_path` / `--controlPlaneHealthCheckCAPath`: CA cert for HTTPS verification
- SIGUSR1 signal handler for runtime configuration dumps (#1301)
  - Send SIGUSR1 to kube-vip process to dump current configuration to stdout
  - Configuration dump includes:
    - Basic configuration (VIP, interface, port, namespace settings)
    - BGP configuration (enabled status, AS number, router ID, peers)
    - ARP/NDP configuration (enabled status, broadcast rate)
    - Services configuration (enabled status, load balancer settings)
    - Network interfaces status
    - Leader election configuration (type, lease details)
    - Runtime statistics (load balancer, Prometheus, health check settings)
  - Output format: Human-readable plaintext via fmt.Printf()
  - Thread-safe implementation using mutex protection
  - Non-disruptive: Process continues running after configuration dump
  - Added comprehensive unit tests for all dump methods
  - Added E2E tests for signal handling

### Changed
- Updated signal handlers in manager_arp.go, manager_bgp.go, manager_wireguard.go, and manager_table.go to use switch statement pattern for handling multiple signals (SIGUSR1, SIGINT, SIGTERM)
- wireguard.go now manages a complete wireguard interface on the current network namespace
- manager_wireguard.go uses the new wireguard.go implementation

## [v1.0.1] - Previous Release

### Previous changes
- See git history for changes prior to CHANGELOG.md introduction
