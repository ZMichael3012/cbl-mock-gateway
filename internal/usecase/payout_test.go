package usecase_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZMichael3012/cbl-kit/money"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
)

// btcAmount builds a BTC money value for the payout tests.
func btcAmount(t *testing.T, literal string) money.Money {
	t.Helper()
	parsed, err := money.Parse(literal, "BTC", 8)
	require.NoError(t, err)
	return parsed
}

// createPullPayment creates a pull payment in the test service.
func createPullPayment(t *testing.T, svc *usecase.Service, name string) domain.PullPayment {
	t.Helper()
	pull, _, err := svc.CreatePullPayment(context.Background(), domain.PullPaymentRequest{
		Name:          name,
		Amount:        btcAmount(t, "0.00100000"),
		PayoutMethods: []string{"BTC"},
	})
	require.NoError(t, err)
	return pull
}

// createPayout creates a payout in the test service.
func createPayout(t *testing.T, svc *usecase.Service, pullPaymentID string) domain.Payout {
	t.Helper()
	payout, err := svc.CreatePayout(context.Background(), pullPaymentID, domain.PayoutRequest{
		Destination: "bcrt1qexample",
		Amount:      btcAmount(t, "0.00100000"),
		MethodID:    "BTC-CHAIN",
	})
	require.NoError(t, err)
	return payout
}

func TestCreatePullPaymentReplayAndValidation(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)

	first := createPullPayment(t, svc, "payout-1")
	assert.Len(t, first.ID, 32, "a btcpay pull payment id is 32 hex characters")

	replayed, replay, err := svc.CreatePullPayment(context.Background(), domain.PullPaymentRequest{
		Name: "payout-1", Amount: btcAmount(t, "0.00100000"), PayoutMethods: []string{"BTC"},
	})
	require.NoError(t, err)
	assert.True(t, replay)
	assert.Equal(t, first.ID, replayed.ID)

	_, _, err = svc.CreatePullPayment(context.Background(), domain.PullPaymentRequest{
		Name: "  ", Amount: btcAmount(t, "0.00100000"),
	})
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
}

func TestCreatePayoutStatesAndValidation(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	pull := createPullPayment(t, svc, "payout-2")
	payout := createPayout(t, svc, pull.ID)
	assert.Equal(t, domain.PayoutStateAwaitingApproval, payout.State)

	_, err := svc.CreatePayout(context.Background(), "missing-pull-payment", domain.PayoutRequest{
		Destination: "bcrt1qexample", Amount: btcAmount(t, "0.00100000"), MethodID: "BTC-CHAIN",
	})
	require.ErrorIs(t, err, domain.ErrPullPaymentNotFound)

	// The payout must be consistent with the pull payment (method and amount).
	_, err = svc.CreatePayout(context.Background(), pull.ID, domain.PayoutRequest{
		Destination: "bcrt1qexample", Amount: btcAmount(t, "0.00100000"), MethodID: "ETH-CHAIN",
	})
	require.ErrorIs(t, err, domain.ErrInvalidRequest)

	_, err = svc.CreatePayout(context.Background(), pull.ID, domain.PayoutRequest{
		Destination: "bcrt1qexample", Amount: btcAmount(t, "0.00200000"), MethodID: "BTC-CHAIN",
	})
	require.ErrorIs(t, err, domain.ErrInvalidRequest)

	_, err = svc.CreatePayout(context.Background(), pull.ID, domain.PayoutRequest{
		Destination: "", Amount: btcAmount(t, "0.00100000"), MethodID: "BTC-CHAIN",
	})
	require.ErrorIs(t, err, domain.ErrInvalidRequest)

	advanced, err := svc.SimulatePayout(context.Background(), payout.ID, domain.PayoutStateCompleted)
	require.NoError(t, err)
	assert.Equal(t, domain.PayoutStateCompleted, advanced.State)

	read, err := svc.GetPayout(context.Background(), payout.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.PayoutStateCompleted, read.State)

	_, err = svc.SimulatePayout(context.Background(), payout.ID, "paid")
	require.ErrorIs(t, err, domain.ErrInvalidRequest)

	_, err = svc.GetPayout(context.Background(), "missing-payout")
	require.ErrorIs(t, err, domain.ErrPayoutNotFound)

	_, err = svc.SimulatePayout(context.Background(), "missing-payout", domain.PayoutStateCompleted)
	require.ErrorIs(t, err, domain.ErrPayoutNotFound)
}

func TestPayoutStoresAreCapped(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	for i := 0; i < 1000; i++ {
		createPullPayment(t, svc, "cap-"+itoa(i))
	}
	_, _, err := svc.CreatePullPayment(context.Background(), domain.PullPaymentRequest{
		Name: "cap-overflow", Amount: btcAmount(t, "0.00100000"), PayoutMethods: []string{"BTC"},
	})
	require.ErrorIs(t, err, domain.ErrStoreFull)

	svcCapped, _ := newService(t)
	pull := createPullPayment(t, svcCapped, "cap-payout")
	for i := 0; i < 1000; i++ {
		createPayout(t, svcCapped, pull.ID)
	}
	_, err = svcCapped.CreatePayout(context.Background(), pull.ID, domain.PayoutRequest{
		Destination: "bcrt1qexample", Amount: btcAmount(t, "0.00100000"), MethodID: "BTC-CHAIN",
	})
	require.ErrorIs(t, err, domain.ErrStoreFull)
}

func TestExportInvoicesFiltersProviderAndWindow(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	svc := usecase.NewService(sender, nil, usecase.WithClock(func() time.Time { return clock }))

	profile, ok := domain.ProfileByID("btcpay")
	require.True(t, ok)
	_, _, err := svc.CreateInvoice(context.Background(), domain.CreateInvoiceRequest{
		Provider:       profile.ID,
		IdempotencyKey: "export-btcpay",
		PaymentID:      testPaymentID,
		Amount:         money.Money{MinorUnits: 100_000, Asset: "BTC", Decimals: 8},
		Network:        "btc",
		ExpiresAt:      clock.Add(15 * time.Minute),
	})
	require.NoError(t, err)
	createInvoice(t, svc, "export-mock")

	rows, err := svc.ExportInvoices(context.Background(), profile.ID, clock.Add(-time.Hour), clock.Add(time.Hour), 0, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1, "only the btcpay invoice belongs to the store list")
	assert.Equal(t, profile.ID, rows[0].Provider)

	empty, err := svc.ExportInvoices(context.Background(), profile.ID, clock.Add(-2*time.Hour), clock.Add(-time.Hour), 0, 0)
	require.NoError(t, err)
	assert.Empty(t, empty)

	// The order lookup serves the connector LookupByOrderID recovery.
	found, err := svc.LookupInvoicesByOrder(context.Background(), profile.ID, testPaymentID)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, profile.ID, found[0].Provider)

	missing, err := svc.LookupInvoicesByOrder(context.Background(), profile.ID, "01a138fd-ffff-7000-8000-00000000ffff")
	require.NoError(t, err)
	assert.Empty(t, missing)
}

// itoa renders an integer for the cap test names.
func itoa(value int) string {
	return strconv.Itoa(value)
}
