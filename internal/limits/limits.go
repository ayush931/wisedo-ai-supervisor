// Package limits defines the Supervisor's request and ingestion size limits in one place
// (WIS-290 "Configuration, limits and data lifecycle": "configure these centrally and align
// WIS-270/272"). Every handler and ingestion path reads these constants instead of repeating
// literals, so a limit cannot drift between packages. They are engineering defaults for the
// first supported deployment, not negotiated customer SLAs.
package limits

import "time"

const (
	// RequestBodyBytes bounds a JSON request body (WIS-290: "1 MiB request body").
	RequestBodyBytes = 1 << 20
	// EventBytes bounds one operational event payload (WIS-290, WIS-272: "Events are bounded
	// to 256 KiB each").
	EventBytes = 256 << 10
	// ImportBytes and ImportRows bound one CSV/JSON import (WIS-290, WIS-272: "CSV import
	// <=10 MiB/50k rows"). Migration 026 also enforces ImportRows on import jobs.
	ImportBytes = 10 << 20
	ImportRows  = 50000
	// ConfigImportRows bounds the site-locking master-data importer; raising it requires a faster
	// import and a re-measured site-lock bound.
	ConfigImportRows = 2000
	// DocumentBytes bounds one uploaded source document (WIS-290, WIS-271: "PDF <=20 MiB",
	// "Limits 20MiB/file").
	DocumentBytes = 20 << 20
	// ExtractedTextBytes bounds the text extracted from one document (WIS-271: "2MiB extracted
	// text").
	ExtractedTextBytes = 2 << 20
	// DecompressedBytes bounds the expanded size of an archive-based document such as DOCX
	// (WIS-271: "decompression beyond 100MiB for DOCX").
	DecompressedBytes = 100 << 20
)

// MaxHeartbeatAge caps heartbeat freshness regardless of a source's configured
// freshness_seconds, which may be longer. Source liveness, plant-context readiness and the
// executor's dispatch-time evidence check all judge heartbeats through HeartbeatLimit, so a
// source never reads live while dispatch refuses it as stale (WIS-272).
const MaxHeartbeatAge = 120 * time.Second

// HeartbeatLimit is the effective heartbeat age limit for a source's freshness_seconds:
// the configured value, capped at MaxHeartbeatAge (a non-positive value also gets the cap).
func HeartbeatLimit(freshnessSeconds int64) time.Duration {
	limit := time.Duration(freshnessSeconds) * time.Second
	if limit <= 0 || limit > MaxHeartbeatAge {
		return MaxHeartbeatAge
	}
	return limit
}
