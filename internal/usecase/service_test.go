package usecase_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-kit/money"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
)

// testPaymentID sets the payment UUID for tests.
const testPaymentID = "01a138fd-0000-7000-8000-000000000002"

// fakeSender records the sent webhooks and returns the configured result.
type fakeSender struct {
	mu       sync.Mutex
	events   []usecase.WebhookEvent
	delivery usecase.Delivery
}

// Send records the event and returns the preconfigured delivery.
func (s *fakeSender) Send(_ context.Context, event usecase.WebhookEvent) usecase.Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return s.delivery
}

// eventsCopy returns a copy of the sent events.
func (s *fakeSender) eventsCopy() []usecase.WebhookEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usecase.WebhookEvent(nil), s.events...)
}

// newService builds a service with a fake sender.
func newService(t *testing.T) (*usecase.Service, *fakeSender) {
	t.Helper()
	sender := &fakeSender{delivery: usecase.Delivery{HTTPStatus: 200, SentAt: time.Now().UTC()}}
	return usecase.NewService(sender, nil), sender
}

// createInvoice creates an invoice in the test service.
func createInvoice(t *testing.T, svc *usecase.Service, key string) domain.Invoice {
	t.Helper()
	invoice, _, err := svc.CreateInvoice(context.Background(), domain.CreateInvoiceRequest{
		IdempotencyKey: key,
		PaymentID:      testPaymentID,
		Amount:         money.Money{MinorUnits: 100_000, Asset: "USDT", Decimals: 2},
		Network:        "trc20",
		ExpiresAt:      time.Now().UTC().Add(15 * time.Minute),
	})
	require.NoError(t, err)
	return invoice
}

func TestCreateInvoiceIdempotency(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	first := createInvoice(t, svc, "key-1")

	assert.NotEmpty(t, first.GatewayID)
	assert.Equal(t, domain.StatusPending, first.Status)
	assert.Contains(t, first.Address, "MOCK")
	assert.Equal(t, testPaymentID, first.PaymentID)

	repeat, replayed, err := svc.CreateInvoice(context.Background(), domain.CreateInvoiceRequest{
		IdempotencyKey: "key-1",
		PaymentID:      testPaymentID,
		Amount:         money.Money{MinorUnits: 100_000, Asset: "USDT", Decimals: 2},
		Network:        "trc20",
		ExpiresAt:      time.Now().UTC().Add(15 * time.Minute),
	})
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first.GatewayID, repeat.GatewayID)

	other := createInvoice(t, svc, "key-2")
	assert.NotEqual(t, first.GatewayID, other.GatewayID)
}

func TestCreateInvoiceValidation(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	_, _, err := svc.CreateInvoice(context.Background(), domain.CreateInvoiceRequest{})
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
}

func TestGetInvoiceNotFound(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	_, err := svc.GetInvoice(context.Background(), "unknown")
	require.ErrorIs(t, err, domain.ErrInvoiceNotFound)
}

func TestInvoiceStoreFull(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	for i := 0; i < 1000; i++ {
		_, _, err := svc.CreateInvoice(context.Background(), domain.CreateInvoiceRequest{
			IdempotencyKey: fmt.Sprintf("key-%d", i),
			PaymentID:      testPaymentID,
			Amount:         money.Money{MinorUnits: 1, Asset: "USDT", Decimals: 2},
			Network:        "trc20",
			ExpiresAt:      time.Now().UTC().Add(time.Minute),
		})
		require.NoError(t, err, i)
	}

	_, _, err := svc.CreateInvoice(context.Background(), domain.CreateInvoiceRequest{
		IdempotencyKey: "key-overflow",
		PaymentID:      testPaymentID,
		Amount:         money.Money{MinorUnits: 1, Asset: "USDT", Decimals: 2},
		Network:        "trc20",
		ExpiresAt:      time.Now().UTC().Add(time.Minute),
	})
	require.ErrorIs(t, err, domain.ErrStoreFull)
}

func TestSimulateUpdatesInvoiceAndDuplicates(t *testing.T) {
	t.Parallel()

	svc, sender := newService(t)
	invoice := createInvoice(t, svc, "key-1")

	result, err := svc.Simulate(context.Background(), domain.SimulateRequest{
		GatewayID: invoice.GatewayID,
		Status:    "confirmed",
		Duplicate: 3,
		Signature: domain.SignatureValid,
	})
	require.NoError(t, err)
	require.Len(t, result.Deliveries, 3)
	assert.Equal(t, 200, result.Deliveries[0].HTTPStatus)

	events := sender.eventsCopy()
	require.Len(t, events, 3)
	for _, event := range events {
		assert.Equal(t, result.EventID, event.EventID, "duplicates use the same event_id")
		assert.Equal(t, domain.StatusConfirmed, event.Status)
		assert.Equal(t, int64(100_000), event.ReceivedAmount.MinorUnits)
		assert.Equal(t, testPaymentID, event.PaymentID)
	}

	updated, err := svc.GetInvoice(context.Background(), invoice.GatewayID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusConfirmed, updated.Status)
	assert.Equal(t, int64(100_000), updated.ReceivedAmount.MinorUnits)
}

func TestSimulateOverrides(t *testing.T) {
	t.Parallel()

	svc, sender := newService(t)
	invoice := createInvoice(t, svc, "key-1")
	eventTS := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	received := money.Money{MinorUnits: 90_000, Asset: "USDT", Decimals: 2}

	_, err := svc.Simulate(context.Background(), domain.SimulateRequest{
		GatewayID:      invoice.GatewayID,
		Status:         "completed",
		ReceivedAmount: &received,
		Confirmations:  6,
		TXID:           "0x123",
		EventTS:        &eventTS,
		Signature:      domain.SignatureValid,
	})
	require.NoError(t, err)

	events := sender.eventsCopy()
	require.Len(t, events, 1)
	assert.Equal(t, domain.StatusConfirmed, events[0].Status, "the completed alias maps to confirmed")
	assert.Equal(t, int64(90_000), events[0].ReceivedAmount.MinorUnits)
	assert.Equal(t, uint32(6), events[0].Confirmations)
	assert.Equal(t, "0x123", events[0].TXID)
	assert.Equal(t, eventTS, events[0].EventTS)
}

func TestSimulateUnknownInvoice(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	_, err := svc.Simulate(context.Background(), domain.SimulateRequest{
		GatewayID: "unknown",
		Status:    "confirmed",
		Signature: domain.SignatureValid,
	})
	require.ErrorIs(t, err, domain.ErrInvoiceNotFound)
}

func TestScenarioFailNext(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	invoice := createInvoice(t, svc, "key-1")
	require.NoError(t, svc.SetScenario(domain.Scenario{FailNext: 2}))

	_, err := svc.GetInvoice(context.Background(), invoice.GatewayID)
	require.ErrorIs(t, err, domain.ErrInjectedFailure)
	_, err = svc.GetInvoice(context.Background(), invoice.GatewayID)
	require.ErrorIs(t, err, domain.ErrInjectedFailure)

	_, err = svc.GetInvoice(context.Background(), invoice.GatewayID)
	require.NoError(t, err, "the API works after fail_next is exhausted")
	assert.Equal(t, 0, svc.GetScenario().FailNext)
}

func TestScenarioLatencyAndReset(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	require.NoError(t, svc.SetScenario(domain.Scenario{LatencyMS: 80}))

	started := time.Now()
	createInvoice(t, svc, "key-1")
	assert.GreaterOrEqual(t, time.Since(started), 70*time.Millisecond)

	svc.ResetScenario()
	assert.Equal(t, domain.Scenario{}, svc.GetScenario())
}

func TestDeliveriesLogCapped(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	invoice := createInvoice(t, svc, "key-1")
	for i := 0; i < 11; i++ {
		_, err := svc.Simulate(context.Background(), domain.SimulateRequest{
			GatewayID: invoice.GatewayID,
			Status:    "detected",
			Duplicate: 10,
			Signature: domain.SignatureValid,
		})
		require.NoError(t, err)
	}

	deliveries := svc.ListDeliveries()
	assert.Len(t, deliveries, 100, "the log is capped at 100 entries")
	assert.Equal(t, invoice.GatewayID, deliveries[0].GatewayID)
}

func TestConcurrentInvoicesAndSimulations(t *testing.T) {
	svc, sender := newService(t)
	invoice := createInvoice(t, svc, "key-1")

	var wg sync.WaitGroup
	var simulations int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				_, _ = svc.GetInvoice(context.Background(), invoice.GatewayID)
				return
			}
			if _, err := svc.Simulate(context.Background(), domain.SimulateRequest{
				GatewayID: invoice.GatewayID,
				Status:    "detected",
				Signature: domain.SignatureValid,
			}); err == nil {
				atomic.AddInt32(&simulations, 1)
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(10), atomic.LoadInt32(&simulations), "all simulate calls completed successfully")
	assert.Len(t, sender.eventsCopy(), 10)
	updated, err := svc.GetInvoice(context.Background(), invoice.GatewayID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusDetected, updated.Status)
}
