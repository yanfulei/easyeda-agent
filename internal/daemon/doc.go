// Package daemon hosts the long-running local server that bridges Skills, the
// CLI, and the EasyEDA connector extension. Phase 1 exposes a /health endpoint
// on the fixed port selected by the CLI; it also owns WebSocket action dispatch,
// artifact storage, write leases, and audit logging.
package daemon
