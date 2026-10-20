package usecase_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZMichael3012/cbl-kit/money"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
)

// profileSender is a WebhookSender stub for the provider profile tests.
type profileSender struct{}

// Send returns a successful delivery.
func (profileSender) Send(_ context.Context, event usecase.WebhookEvent) usecase.Delivery {
	return usecase.Delivery{HTTPStatus: 200, SentAt: time.Now().UTC(), Provider: event.Provider}
}

// profileTargets resolves targets for the profile tests.
type profileTargets struct {
	missing bool
}

// HasTarget reports whether the profile has a target.
func (t profileTargets) HasTarget(domain.ProfileID) bool { return !t.missing }

// profileCreateRequest builds a provider creation request.
func profileCreateRequest(provider domain.ProfileID, orderID string) domain.CreateInvoiceRequest {
	return domain.CreateInvoiceRequest{
		Provider:       provider,
		IdempotencyKey: orderID,
		PaymentID:      orderID,
		Amount:         money.MustParse("12.34", "USDT", 6),
		Network:        "usdt_trc20",
		ExpiresAt:      time.Date(2026, 10, 17, 13, 0, 0, 0, time.UTC),
	}
}

func TestCreateInvoiceProviderIDs(t *testing.T) {
	service := usecase.NewService(profileSender{}, nil)
	ctx := context.Background()
	orderID := "01a138fd-0000-7000-8000-000000000041"

	now, _, err := service.CreateInvoice(ctx, profileCreateRequest(domain.ProfileNOWPayments, orderID))
	require.NoError(t, err)
	assert.Len(t, now.GatewayID, 36)
	assert.Equal(t, domain.ProfileNOWPayments, now.Provider)
	assert.Equal(t, "waiting", now.RawStatus)

	coinGate, replayed, err := service.CreateInvoice(ctx, profileCreateRequest(domain.ProfileCoinGate, orderID))
	require.NoError(t, err)
	assert.False(t, replayed)
	_, err = strconv.ParseInt(coinGate.GatewayID, 10, 64)
	assert.NoError(t, err, "the coin gate id must be numeric")

	replay, replayed, err := service.CreateInvoice(ctx, profileCreateRequest(domain.ProfileCoinGate, orderID))
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, coinGate.GatewayID, replay.GatewayID)

	bitPay, _, err := service.CreateInvoice(ctx, profileCreateRequest(domain.ProfileBitPay, orderID))
	require.NoError(t, err)
	assert.Len(t, bitPay.GatewayID, 22)

	btcPay, _, err := service.CreateInvoice(ctx, profileCreateRequest(domain.ProfileBTCPay, orderID))
	require.NoError(t, err)
	assert.Len(t, btcPay.GatewayID, 32)

	mock, _, err := service.CreateInvoice(ctx, profileCreateRequest("", orderID))
	require.NoError(t, err)
	assert.Equal(t, domain.ProfileMock, mock.Provider)
	assert.Equal(t, "pending", mock.RawStatus)

	_, _, err = service.CreateInvoice(ctx, profileCreateRequest("unknown", orderID))
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "unknown provider"))
}

func TestSimulateProviderPaths(t *testing.T) {
	service := usecase.NewService(profileSender{}, nil, usecase.WithTargets(profileTargets{}))
	ctx := context.Background()
	orderID := "01a138fd-0000-7000-8000-000000000042"
	invoice, _, err := service.CreateInvoice(ctx, profileCreateRequest(domain.ProfileNOWPayments, orderID))
	require.NoError(t, err)

	result, err := service.Simulate(ctx, domain.SimulateRequest{
		GatewayID: invoice.GatewayID,
		Provider:  domain.ProfileNOWPayments,
		Status:    "confirmed",
		Signature: domain.SignatureValid,
	})
	require.NoError(t, err)
	require.Len(t, result.Deliveries, 1)
	assert.Equal(t, domain.ProfileNOWPayments, result.Deliveries[0].Provider)

	stored, err := service.GetInvoice(ctx, invoice.GatewayID)
	require.NoError(t, err)
	assert.Equal(t, "finished", stored.RawStatus)

	_, err = service.Simulate(ctx, domain.SimulateRequest{
		GatewayID: invoice.GatewayID,
		Status:    "confirmed",
		Signature: domain.SignatureValid,
	})
	require.Error(t, err, "the mock default provider must not simulate a nowpayments invoice")
	assert.Contains(t, err.Error(), "provider_mismatch")

	_, err = service.Simulate(ctx, domain.SimulateRequest{
		GatewayID: invoice.GatewayID,
		Provider:  domain.ProfileNOWPayments,
		Status:    "confirmed",
		RawStatus: "not-a-status",
		Signature: domain.SignatureValid,
	})
	require.Error(t, err)
}

func TestSimulateMissingTargetError(t *testing.T) {
	service := usecase.NewService(profileSender{}, nil, usecase.WithTargets(profileTargets{missing: true}))
	ctx := context.Background()
	orderID := "01a138fd-0000-7000-8000-000000000043"
	invoice, _, err := service.CreateInvoice(ctx, profileCreateRequest(domain.ProfileCoinGate, orderID))
	require.NoError(t, err)

	_, err = service.Simulate(ctx, domain.SimulateRequest{
		GatewayID: invoice.GatewayID,
		Provider:  domain.ProfileCoinGate,
		Status:    "confirmed",
		Signature: domain.SignatureValid,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing_webhook_target")
}

func TestOptionsCoverage(t *testing.T) {
	fixed := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	service := usecase.NewService(profileSender{}, nil,
		usecase.WithClock(func() time.Time { return fixed }),
		usecase.WithTargets(profileTargets{}),
	)
	orderID := "01a138fd-0000-7000-8000-000000000044"
	invoice, _, err := service.CreateInvoice(context.Background(), profileCreateRequest("", orderID))
	require.NoError(t, err)
	assert.Equal(t, fixed, invoice.CreatedAt)

	service = usecase.NewService(profileSender{}, nil, usecase.WithClock(nil), usecase.WithTargets(nil))
	_, _, err = service.CreateInvoice(context.Background(), profileCreateRequest("", orderID))
	require.NoError(t, err)
}
