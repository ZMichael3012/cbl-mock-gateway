package webhook_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-kit/hmac"
	"github.com/cbl/cbl-kit/money"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
	"github.com/cbl/cbl-mock-gateway/internal/webhook"
)

const (
	unitSecret = "unit-secret"
	unitSig    = "X-Billing-Signature"
	unitEvent  = "X-Event-Id"
)

// unitWebhookEvent builds a webhook event for tests.
func unitWebhookEvent(mode domain.SignatureMode) usecase.WebhookEvent {
	return usecase.WebhookEvent{
		EventID:        "01a138fd-0000-7000-8000-000000000001",
		PaymentID:      "01a138fd-0000-7000-8000-000000000002",
		GatewayID:      "gw-1",
		Status:         domain.StatusConfirmed,
		ReceivedAmount: money.Money{MinorUnits: 100_000, Asset: "USDT", Decimals: 2},
		Confirmations:  4,
		TXID:           "0xabc",
		Network:        "trc20",
		EventTS:        time.Date(2026, 10, 14, 12, 0, 5, 0, time.UTC),
		Mode:           mode,
	}
}

// payloadReader builds a webhook handler that records the request.
type payloadReader struct {
	mu       sync.Mutex
	body     []byte
	sigOK    bool
	eventID  string
	requests int
}

// ServeHTTP implements webhook reception with signature verification.
func (s *payloadReader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	s.requests++
	s.body = body
	s.eventID = r.Header.Get(unitEvent)
	s.sigOK = hmac.Verify([]byte(unitSecret), r.Header.Get(unitSig), body, time.Now(), hmac.DefaultWindow) == nil
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// requestCount returns the number of received requests.
func (s *payloadReader) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func TestNewHTTPSenderValidation(t *testing.T) {
	t.Parallel()

	_, err := webhook.NewHTTPSender("not-a-url", unitSecret, unitSig, unitEvent, time.Second)
	require.ErrorIs(t, err, domain.ErrInvalidRequest)

	_, err = webhook.NewHTTPSender("ftp://example.com", unitSecret, unitSig, unitEvent, time.Second)
	require.ErrorIs(t, err, domain.ErrInvalidRequest)

	_, err = webhook.NewHTTPSender("http://example.com/hook", " ", unitSig, unitEvent, time.Second)
	require.ErrorIs(t, err, domain.ErrInvalidRequest)

	_, err = webhook.NewHTTPSender("http://example.com/hook", unitSecret, " ", unitEvent, time.Second)
	require.ErrorIs(t, err, domain.ErrInvalidRequest)

	sender, err := webhook.NewHTTPSender("http://example.com/hook", unitSecret, unitSig, unitEvent, 0)
	require.NoError(t, err)
	require.NotNil(t, sender)
}

func TestSendValidSignatureAndContract(t *testing.T) {
	t.Parallel()

	sink := &payloadReader{}
	server := httptest.NewServer(sink)
	defer server.Close()

	sender, err := webhook.NewHTTPSender(server.URL, unitSecret, unitSig, unitEvent, 2*time.Second)
	require.NoError(t, err)

	delivery := sender.Send(context.Background(), unitWebhookEvent(domain.SignatureValid))
	assert.Equal(t, http.StatusOK, delivery.HTTPStatus)
	assert.Empty(t, delivery.Error)
	assert.False(t, delivery.SentAt.IsZero())

	sink.mu.Lock()
	defer sink.mu.Unlock()
	assert.True(t, sink.sigOK, "the signature passes kit hmac.Verify")
	assert.Equal(t, "01a138fd-0000-7000-8000-000000000001", sink.eventID)

	var body map[string]any
	require.NoError(t, json.Unmarshal(sink.body, &body))
	assert.Equal(t, "01a138fd-0000-7000-8000-000000000002", body["payment_id"])
	assert.Equal(t, "gw-1", body["gateway_id"])
	assert.Equal(t, "confirmed", body["status"])
	assert.Equal(t, "USDT", body["received_amount"].(map[string]any)["asset"])
	assert.Equal(t, float64(100_000), body["received_amount"].(map[string]any)["minor_units"])
	assert.Equal(t, "0xabc", body["txid"])
	assert.Equal(t, "trc20", body["network"])
	_, err = time.Parse(time.RFC3339Nano, body["event_ts"].(string))
	assert.NoError(t, err)
}

func TestSendSignatureModes(t *testing.T) {
	t.Parallel()

	sink := &payloadReader{}
	server := httptest.NewServer(sink)
	defer server.Close()

	sender, err := webhook.NewHTTPSender(server.URL, unitSecret, unitSig, unitEvent, 2*time.Second)
	require.NoError(t, err)

	sender.Send(context.Background(), unitWebhookEvent(domain.SignatureInvalid))
	sink.mu.Lock()
	assert.False(t, sink.sigOK, "a corrupted signature is rejected")
	sink.mu.Unlock()

	sender.Send(context.Background(), unitWebhookEvent(domain.SignatureMissing))
	sink.mu.Lock()
	assert.False(t, sink.sigOK, "a webhook without a signature is rejected")
	assert.Equal(t, 2, sink.requests)
	sink.mu.Unlock()
}

func TestSendNetworkFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.NotFoundHandler())
	serverURL := server.URL
	server.Close()

	sender, err := webhook.NewHTTPSender(serverURL, unitSecret, unitSig, unitEvent, time.Second)
	require.NoError(t, err)

	delivery := sender.Send(context.Background(), unitWebhookEvent(domain.SignatureValid))
	assert.Zero(t, delivery.HTTPStatus, "a network failure yields no HTTP status")
	assert.NotEmpty(t, delivery.Error)
}

func TestSendRedirectNotFollowed(t *testing.T) {
	t.Parallel()

	redirected := &payloadReader{}
	redirectSrv := httptest.NewServer(redirected)
	defer redirectSrv.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectSrv.URL, http.StatusFound)
	}))
	defer redirector.Close()

	sender, err := webhook.NewHTTPSender(redirector.URL, unitSecret, unitSig, unitEvent, time.Second)
	require.NoError(t, err)

	delivery := sender.Send(context.Background(), unitWebhookEvent(domain.SignatureValid))
	assert.Equal(t, http.StatusFound, delivery.HTTPStatus)
	assert.Contains(t, delivery.Error, "302")
	assert.Equal(t, 0, redirected.requestCount(), "the redirect is not followed, the body is not sent to another host")
}
