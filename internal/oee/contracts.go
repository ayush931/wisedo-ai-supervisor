package oee

import (
	"time"

	"github.com/google/uuid"
)

// ProblemDetails represents an RFC 9457 problem details response.
type ProblemDetails struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Code     string `json:"code"`
	Detail   string `json:"detail"`
	Instance string `json:"instance,omitempty"`
	TraceID  string `json:"trace_id,omitempty"`
}

// RequestContext represents caller context.
type RequestContext struct {
	TenantID      string   `json:"tenant_id"`
	SiteID        string   `json:"site_id"`
	UserID        string   `json:"user_id"`
	Roles         []string `json:"roles"`
	CorrelationID string   `json:"correlation_id"`
	RequestID     string   `json:"request_id"`
	ClientIP      string   `json:"client_ip,omitempty"`
	Authenticated bool     `json:"authenticated"`
}

// CalculationRequestV1 is the payload submitted to request an OEE calculation.
type CalculationRequestV1 struct {
	DatasetID      uuid.UUID `json:"dataset_id"`
	PolicyID       uuid.UUID `json:"policy_id"`
	Scope          string    `json:"scope" contract:"max=64"`
	WindowStart    string    `json:"window_start" contract:"datetime"`
	WindowEnd      string    `json:"window_end" contract:"datetime"`
	IdempotencyKey string    `json:"idempotency_key" contract:"max=128"`
}

// CalculationAcceptedV1 is returned when a calculation is accepted.
type CalculationAcceptedV1 struct {
	RunID     uuid.UUID `json:"run_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// CalculationStatusV1 represents run lifecycle.
type CalculationStatusV1 struct {
	RunID      uuid.UUID  `json:"run_id"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	EvidenceID *uuid.UUID `json:"evidence_id,omitempty"`
}

// DatasetV1 represents an immutable dataset input. It carries no tenant: tenant identity
// comes from verified authentication, never from a payload (WIS-225, rule 03).
type DatasetV1 struct {
	DatasetID uuid.UUID `json:"dataset_id"`
	SiteID    string    `json:"site_id"`
	AssetID   string    `json:"asset_id"`
	Digest    string    `json:"digest" contract:"sha256"`
}

// PolicyV1 represents an immutable policy input. It carries no tenant, like DatasetV1.
type PolicyV1 struct {
	PolicyID uuid.UUID `json:"policy_id"`
	SiteID   string    `json:"site_id"`
	Digest   string    `json:"digest" contract:"sha256"`
}

// EvidencePackV1 represents sealed evidence output.
type EvidencePackV1 struct {
	EvidenceID    uuid.UUID    `json:"evidence_id"`
	RunID         uuid.UUID    `json:"run_id"`
	DatasetDigest string       `json:"dataset_digest" contract:"sha256"`
	PolicyDigest  string       `json:"policy_digest" contract:"sha256"`
	Availability  string       `json:"availability,omitempty" contract:"decimal6"`
	Performance   string       `json:"performance,omitempty" contract:"decimal6"`
	Quality       string       `json:"quality,omitempty" contract:"decimal6"`
	OEE           string       `json:"oee,omitempty" contract:"decimal6"`
	Status        Status       `json:"status"`
	ReasonCodes   []ReasonCode `json:"reason_codes"`
	SealedAtUTC   time.Time    `json:"sealed_at_utc"`
}
