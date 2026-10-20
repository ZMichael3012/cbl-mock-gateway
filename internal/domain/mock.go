package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ZMichael3012/cbl-kit/money"
)

// Domain errors of the mock-gateway.
var (
	// ErrInvalidRequest reports an invalid request.
	ErrInvalidRequest = errors.New("mock-gateway: invalid request")
	// ErrInvoiceNotFound reports an unknown mock provider invoice.
	ErrInvoiceNotFound = errors.New("mock-gateway: invoice not found")
	// ErrInjectedFailure reports an injected provider failure scenario.
	ErrInjectedFailure = errors.New("mock-gateway: injected provider failure")
	// ErrStoreFull reports an in-memory invoice store overflow.
	ErrStoreFull = errors.New("mock-gateway: invoice store is full")
	// ErrUnauthorized reports a missing or malformed profile auth header.
	ErrUnauthorized = errors.New("mock-gateway: unauthorized")
)

// InjectedFailureError carries the scenario response of an injected provider
// failure: the HTTP status (503 by default) and the optional Retry-After.
type InjectedFailureError struct {
	// Status sets the response status of the injected failure.
	Status int
	// RetryAfterS sets the Retry-After value in seconds (429 only; 0 omits the header).
	RetryAfterS int
}

// Error implements the error interface.
func (e InjectedFailureError) Error() string {
	return fmt.Sprintf("%s: status %d", ErrInjectedFailure.Error(), e.Status)
}

// Unwrap returns the sentinel error for errors.Is.
func (e InjectedFailureError) Unwrap() error {
	return ErrInjectedFailure
}

// Status describes the status of a mock provider invoice and event.
type Status string

// Invoice and event statuses of the mock provider.
const (
	// StatusPending describes a created but unpaid invoice.
	StatusPending Status = "pending"
	// StatusDetected describes a detected payment.
	StatusDetected Status = "detected"
	// StatusConfirmed describes a confirmed payment.
	StatusConfirmed Status = "confirmed"
	// StatusFailed describes a failed payment.
	StatusFailed Status = "failed"
	// StatusExpired describes an expired invoice.
	StatusExpired Status = "expired"
	// StatusUnderpaid describes an underpayment.
	StatusUnderpaid Status = "underpaid"
	// StatusOverpaid describes an overpayment.
	StatusOverpaid Status = "overpaid"
)

// statusAliases maps simulate strings to canonical event statuses,
// repeating the connector alias table (CBL-3.1) so that tests can
// use provider wording.
var statusAliases = map[string]Status{
	"detected":  StatusDetected,
	"pending":   StatusDetected,
	"seen":      StatusDetected,
	"confirmed": StatusConfirmed,
	"completed": StatusConfirmed,
	"success":   StatusConfirmed,
	"failed":    StatusFailed,
	"error":     StatusFailed,
	"expired":   StatusExpired,
	"underpaid": StatusUnderpaid,
	"overpaid":  StatusOverpaid,
}

// CanonicalStatus converts a simulate status string into a canonical event status.
func CanonicalStatus(raw string) (Status, error) {
	status, ok := statusAliases[strings.ToLower(strings.TrimSpace(raw))]
	if !ok {
		return "", fmt.Errorf("%w: status %q", ErrInvalidRequest, raw)
	}
	return status, nil
}

// SignatureMode describes the webhook signature mode for failure scenarios.
type SignatureMode string

// Webhook signature modes.
const (
	// SignatureValid sets a correct signature.
	SignatureValid SignatureMode = "valid"
	// SignatureInvalid sets a corrupted signature.
	SignatureInvalid SignatureMode = "invalid"
	// SignatureMissing sets a missing signature header.
	SignatureMissing SignatureMode = "missing"
)

// NormalizeSignature converts a signature mode string into a value; empty means valid.
func NormalizeSignature(raw string) (SignatureMode, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return SignatureValid, nil
	}
	switch SignatureMode(trimmed) {
	case SignatureValid, SignatureInvalid, SignatureMissing:
		return SignatureMode(trimmed), nil
	default:
		return "", fmt.Errorf("%w: signature mode %q", ErrInvalidRequest, raw)
	}
}

// Mock provider field limits: scenarios are bounded by handler budgets
// (WriteTimeout) and the request body limit (spec NFR).
const (
	maxAssetLen      = 16
	minAssetLen      = 2
	maxNetworkLen    = 32
	maxTXIDLen       = 128
	maxNameLen       = 128
	maxAddressLen    = 256
	maxPayoutMethods = 10
	maxLatencyMS     = 5_000
	maxFailNext      = 100
	maxDuplicates    = 10
	maxDelayMS       = 5_000
)

// CreateInvoiceRequest describes an invoice creation request.
type CreateInvoiceRequest struct {
	// Provider sets the provider profile (empty means the mock profile).
	Provider ProfileID
	// IdempotencyKey sets the idempotency key of the call.
	IdempotencyKey string
	// PaymentID sets the platform payment.
	PaymentID string
	// Amount sets the invoice amount.
	Amount money.Money
	// Network sets the payment network.
	Network string
	// ExpiresAt sets the invoice expiration time.
	ExpiresAt time.Time
}

// Validate checks the required fields of the invoice creation request.
func (r CreateInvoiceRequest) Validate() error {
	if _, err := uuid.Parse(strings.TrimSpace(r.PaymentID)); err != nil {
		return fmt.Errorf("%w: payment_id must be uuid", ErrInvalidRequest)
	}
	if _, err := money.New(r.Amount.MinorUnits, r.Amount.Asset, r.Amount.Decimals); err != nil {
		return fmt.Errorf("%w: amount: %w", ErrInvalidRequest, err)
	}
	if r.Amount.MinorUnits <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrInvalidRequest)
	}
	if !validCode(r.Amount.Asset, minAssetLen, maxAssetLen) {
		return fmt.Errorf("%w: invalid amount asset", ErrInvalidRequest)
	}
	if !validCode(r.Network, 1, maxNetworkLen) {
		return fmt.Errorf("%w: invalid network", ErrInvalidRequest)
	}
	if r.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: empty expires_at", ErrInvalidRequest)
	}
	return nil
}

// Invoice describes a mock provider invoice.
type Invoice struct {
	// GatewayID sets the provider invoice identifier.
	GatewayID string
	// Provider sets the provider profile of the invoice.
	Provider ProfileID
	// PaymentID sets the platform payment.
	PaymentID string
	// Address sets the mock payment address.
	Address string
	// Network sets the payment network.
	Network string
	// Amount sets the expected invoice amount.
	Amount money.Money
	// Status sets the current invoice status.
	Status Status
	// RawStatus sets the provider raw status carried on the wire.
	RawStatus string
	// ReceivedAmount sets the received amount.
	ReceivedAmount money.Money
	// Confirmations sets the confirmation depth.
	Confirmations uint32
	// TXID sets the hash of the last transaction.
	TXID string
	// EventTS sets the time of the last event.
	EventTS time.Time
	// ExpiresAt sets the invoice expiration time.
	ExpiresAt time.Time
	// CreatedAt sets the invoice creation time.
	CreatedAt time.Time
}

// SimulateRequest describes a webhook generation request.
type SimulateRequest struct {
	// GatewayID sets the invoice the event is sent for.
	GatewayID string
	// Provider sets the provider profile (empty means the mock profile).
	Provider ProfileID
	// Status sets the event status (canonical or alias).
	Status string
	// RawStatus overrides the provider raw status; empty means the profile mapping.
	RawStatus string
	// ReceivedAmount sets the received amount; nil means the invoice amount.
	ReceivedAmount *money.Money
	// Confirmations sets the confirmation depth.
	Confirmations uint32
	// TXID sets the transaction hash.
	TXID string
	// EventTS sets the event time; nil means the current time.
	EventTS *time.Time
	// DelayMS sets the send delay in milliseconds.
	DelayMS int
	// Duplicate sets the number of event sends; 0 means one.
	Duplicate int
	// Signature sets the signature mode.
	Signature SignatureMode
	// Silent updates the invoice state without any webhook delivery (T-MOCK-10).
	Silent bool
}

// Validate checks the fields of the webhook generation request.
func (r SimulateRequest) Validate() error {
	if strings.TrimSpace(r.GatewayID) == "" {
		return fmt.Errorf("%w: empty gateway_id", ErrInvalidRequest)
	}
	if _, err := CanonicalStatus(r.Status); err != nil {
		return err
	}
	profile, ok := ProfileByID(string(r.Provider))
	if r.Provider == "" {
		profile, ok = ProfileByID(string(ProfileMock))
	}
	if !ok {
		return fmt.Errorf("%w: unknown provider %q", ErrInvalidRequest, r.Provider)
	}
	if r.RawStatus != "" && !profile.HasRawStatus(r.RawStatus) {
		return fmt.Errorf("%w: unknown_raw_status %q for provider %q", ErrInvalidRequest, r.RawStatus, profile.ID)
	}
	if r.ReceivedAmount != nil {
		if _, err := money.New(r.ReceivedAmount.MinorUnits, r.ReceivedAmount.Asset, r.ReceivedAmount.Decimals); err != nil {
			return fmt.Errorf("%w: received_amount: %w", ErrInvalidRequest, err)
		}
		if r.ReceivedAmount.MinorUnits < 0 {
			return fmt.Errorf("%w: negative received_amount", ErrInvalidRequest)
		}
		if !validCode(r.ReceivedAmount.Asset, minAssetLen, maxAssetLen) {
			return fmt.Errorf("%w: invalid received_amount asset", ErrInvalidRequest)
		}
	}
	if len(r.TXID) > maxTXIDLen {
		return fmt.Errorf("%w: txid too long", ErrInvalidRequest)
	}
	if r.DelayMS < 0 || r.DelayMS > maxDelayMS {
		return fmt.Errorf("%w: delay_ms out of range", ErrInvalidRequest)
	}
	if r.Duplicate < 0 || r.Duplicate > maxDuplicates {
		return fmt.Errorf("%w: duplicate out of range", ErrInvalidRequest)
	}
	if r.Signature != SignatureValid && r.Signature != SignatureInvalid && r.Signature != SignatureMissing {
		return fmt.Errorf("%w: signature mode %q", ErrInvalidRequest, r.Signature)
	}
	return nil
}

// Scenario describes a provider API failure scenario.
type Scenario struct {
	// FailNext sets the number of following calls that answer with an error.
	FailNext int
	// LatencyMS sets the added response latency.
	LatencyMS int
	// FailStatus selects the injected status: 0 means 503;
	// 400/422 inject the provider rejection class (the payout compensation
	// scenario, CBL-17.27), 429/500/502/503 the unavailability class.
	FailStatus int
	// RetryAfterS sets the Retry-After header value in seconds (429 only).
	RetryAfterS int
}

// Validate checks the scenario parameters.
func (s Scenario) Validate() error {
	if s.FailNext < 0 || s.FailNext > maxFailNext {
		return fmt.Errorf("%w: fail_next out of range", ErrInvalidRequest)
	}
	if s.LatencyMS < 0 || s.LatencyMS > maxLatencyMS {
		return fmt.Errorf("%w: latency_ms out of range", ErrInvalidRequest)
	}
	switch s.FailStatus {
	case 0, 400, 422, 429, 500, 502, 503:
	default:
		return fmt.Errorf("%w: fail_status %d is not allowed", ErrInvalidRequest, s.FailStatus)
	}
	if s.RetryAfterS < 0 || s.RetryAfterS > 600 {
		return fmt.Errorf("%w: retry_after_s out of range", ErrInvalidRequest)
	}
	if s.RetryAfterS > 0 && s.FailStatus != 429 {
		return fmt.Errorf("%w: retry_after_s requires fail_status 429", ErrInvalidRequest)
	}
	return nil
}

// WebhookDelivery describes a webhook delivery log entry.
type WebhookDelivery struct {
	// EventID sets the webhook event identifier.
	EventID string
	// GatewayID sets the provider invoice.
	GatewayID string
	// Provider sets the provider profile of the delivery.
	Provider ProfileID
	// Status sets the event status.
	Status Status
	// Mode sets the signature mode.
	Mode SignatureMode
	// SentAt sets the send time.
	SentAt time.Time
	// HTTPStatus sets the receiver response code; 0 means a network failure.
	HTTPStatus int
	// Error sets the delivery error text.
	Error string
}

// validCode checks a code (asset, network): Latin letters, digits, hyphen and underscore.
func validCode(value string, minLen, maxLen int) bool {
	if len(value) < minLen || len(value) > maxLen {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '-', b == '_':
		default:
			return false
		}
	}
	return true
}
