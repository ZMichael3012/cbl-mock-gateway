package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-kit/money"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
)

// testPaymentID задает UUID платежа для тестов.
const testPaymentID = "01a138fd-0000-7000-8000-000000000001"

// validCreateRequest собирает корректный запрос создания счета.
func validCreateRequest() domain.CreateInvoiceRequest {
	return domain.CreateInvoiceRequest{
		IdempotencyKey: "key-1",
		PaymentID:      testPaymentID,
		Amount:         money.Money{MinorUnits: 100_000, Asset: "USDT", Decimals: 2},
		Network:        "trc20",
		ExpiresAt:      time.Date(2026, 10, 14, 13, 0, 0, 0, time.UTC),
	}
}

func TestCreateInvoiceRequestValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, validCreateRequest().Validate())

	cases := map[string]func(*domain.CreateInvoiceRequest){
		"bad payment_id": func(r *domain.CreateInvoiceRequest) { r.PaymentID = "pay-1" },
		"zero amount":    func(r *domain.CreateInvoiceRequest) { r.Amount.MinorUnits = 0 },
		"negative":       func(r *domain.CreateInvoiceRequest) { r.Amount.MinorUnits = -5 },
		"bad asset":      func(r *domain.CreateInvoiceRequest) { r.Amount.Asset = "US DT" },
		"long asset":     func(r *domain.CreateInvoiceRequest) { r.Amount.Asset = "VERYLONGASSETNAME" },
		"bad network":    func(r *domain.CreateInvoiceRequest) { r.Network = "trc 20" },
		"empty network":  func(r *domain.CreateInvoiceRequest) { r.Network = "" },
		"zero expires":   func(r *domain.CreateInvoiceRequest) { r.ExpiresAt = time.Time{} },
		"bad decimals":   func(r *domain.CreateInvoiceRequest) { r.Amount.Decimals = 19 },
	}
	for name, mutate := range cases {
		request := validCreateRequest()
		mutate(&request)
		assert.ErrorIs(t, request.Validate(), domain.ErrInvalidRequest, name)
	}
}

func TestCanonicalStatus(t *testing.T) {
	t.Parallel()

	cases := map[string]domain.Status{
		"detected":  domain.StatusDetected,
		"pending":   domain.StatusDetected,
		"completed": domain.StatusConfirmed,
		"SUCCESS":   domain.StatusConfirmed,
		"error":     domain.StatusFailed,
		"expired":   domain.StatusExpired,
		"underpaid": domain.StatusUnderpaid,
		"overpaid":  domain.StatusOverpaid,
	}
	for raw, want := range cases {
		status, err := domain.CanonicalStatus(raw)
		require.NoError(t, err, raw)
		assert.Equal(t, want, status, raw)
	}

	_, err := domain.CanonicalStatus("weird")
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
}

func TestNormalizeSignature(t *testing.T) {
	t.Parallel()

	valid, err := domain.NormalizeSignature("")
	require.NoError(t, err)
	assert.Equal(t, domain.SignatureValid, valid)

	invalid, err := domain.NormalizeSignature("INVALID")
	require.NoError(t, err)
	assert.Equal(t, domain.SignatureInvalid, invalid)

	missing, err := domain.NormalizeSignature("missing")
	require.NoError(t, err)
	assert.Equal(t, domain.SignatureMissing, missing)

	_, err = domain.NormalizeSignature("other")
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
}

func TestSimulateRequestValidate(t *testing.T) {
	t.Parallel()

	valid := domain.SimulateRequest{GatewayID: "gw-1", Status: "confirmed", Signature: domain.SignatureValid}
	require.NoError(t, valid.Validate())

	cases := map[string]func(*domain.SimulateRequest){
		"empty gateway":  func(r *domain.SimulateRequest) { r.GatewayID = " " },
		"bad status":     func(r *domain.SimulateRequest) { r.Status = "weird" },
		"negative delay": func(r *domain.SimulateRequest) { r.DelayMS = -1 },
		"big delay":      func(r *domain.SimulateRequest) { r.DelayMS = 5_001 },
		"big duplicate":  func(r *domain.SimulateRequest) { r.Duplicate = 11 },
		"bad mode":       func(r *domain.SimulateRequest) { r.Signature = "other" },
		"long txid":      func(r *domain.SimulateRequest) { r.TXID = strings.Repeat("t", 129) },
		"negative amount": func(r *domain.SimulateRequest) {
			amount := money.Money{MinorUnits: -1, Asset: "USDT", Decimals: 2}
			r.ReceivedAmount = &amount
		},
		"bad amount asset": func(r *domain.SimulateRequest) {
			amount := money.Money{MinorUnits: 1, Asset: "US DT", Decimals: 2}
			r.ReceivedAmount = &amount
		},
	}
	for name, mutate := range cases {
		request := valid
		mutate(&request)
		assert.ErrorIs(t, request.Validate(), domain.ErrInvalidRequest, name)
	}
}

func TestScenarioValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, domain.Scenario{FailNext: 2, LatencyMS: 100}.Validate())
	assert.ErrorIs(t, domain.Scenario{FailNext: -1}.Validate(), domain.ErrInvalidRequest)
	assert.ErrorIs(t, domain.Scenario{FailNext: 101}.Validate(), domain.ErrInvalidRequest)
	assert.ErrorIs(t, domain.Scenario{LatencyMS: -1}.Validate(), domain.ErrInvalidRequest)
	assert.ErrorIs(t, domain.Scenario{LatencyMS: 5_001}.Validate(), domain.ErrInvalidRequest)
}
