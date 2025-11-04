# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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

## [v1.0.1] - Previous Release

### Previous changes
- See git history for changes prior to CHANGELOG.md introduction
