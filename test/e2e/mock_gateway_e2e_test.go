//go:build e2e

// Package e2e contains the mock-gateway end-to-end scenario: invoice creation,
// signed webhooks (valid, duplicates, corrupted), ordering and failure
// scenarios. The receiver verifies the kit hmac signature and deduplicates
// events the same way as Webhook Ingress.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZMichael3012/cbl-kit/hmac"

	"github.com/cbl/cbl-mock-gateway/internal/transport"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
	"github.com/cbl/cbl-mock-gateway/internal/webhook"
)

const (
	e2eSecret    = "e2e-secret"
	sigHeader    = "X-Billing-Signature"
	idHeader     = "X-Event-Id"
	e2ePaymentID = "01a138fd-0000-7000-8000-000000000004"
)

// sandboxBody describes the webhook body under the sandbox provider contract.
type sandboxBody struct {
	PaymentID      string `json:"payment_id"`
	GatewayID      string `json:"gateway_id"`
	Status         string `json:"status"`
	ReceivedAmount struct {
		MinorUnits int64  `json:"minor_units"`
		Asset      string `json:"asset"`
		Decimals   uint32 `json:"decimals"`
	} `json:"received_amount"`
	Confirmations uint32 `json:"confirmations"`
	TXID          string `json:"txid"`
	Network       string `json:"network"`
	EventTS       string `json:"event_ts"`
}

// sinkRecord describes an accepted webhook.
type sinkRecord struct {
	eventID   string
	verified  bool
	duplicate bool
	body      sandboxBody
}

// sink accepts webhooks: it verifies the signature and deduplicates by event_id.
type sink struct {
	mu      sync.Mutex
	secret  []byte
	records []sinkRecord
	seen    map[string]bool
}

// ServeHTTP implements webhook reception.
func (s *sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	verified := hmac.Verify(s.secret, r.Header.Get(sigHeader), body, time.Now(), hmac.DefaultWindow) == nil
	var parsed sandboxBody
	_ = json.Unmarshal(body, &parsed)
	eventID := r.Header.Get(idHeader)

	s.mu.Lock()
	duplicate := s.seen[eventID]
	s.seen[eventID] = true
	s.records = append(s.records, sinkRecord{
		eventID:   eventID,
		verified:  verified,
		duplicate: duplicate,
		body:      parsed,
	})
	s.mu.Unlock()

	if !verified {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":"rejected"}`))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status":"accepted","duplicate":%t}`, duplicate)
}

// recordsCopy returns a copy of the accepted records.
func (s *sink) recordsCopy() []sinkRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkRecord(nil), s.records...)
}

// postJSON performs a JSON request against the stand.
func postJSON(t *testing.T, method, url, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	require.NoError(t, err)
	return response.StatusCode, payload
}

// gatewayIDFrom extracts gateway_id from the response.
func gatewayIDFrom(t *testing.T, payload []byte) string {
	t.Helper()
	var response struct {
		GatewayID string `json:"gateway_id"`
	}
	require.NoError(t, json.Unmarshal(payload, &response))
	require.NotEmpty(t, response.GatewayID)
	return response.GatewayID
}

func TestE2EMockGatewayWebhookContract(t *testing.T) {
	recvr := &sink{secret: []byte(e2eSecret), seen: map[string]bool{}}
	sinkSrv := httptest.NewServer(recvr)
	defer sinkSrv.Close()

	sender, err := webhook.NewHTTPSender(sinkSrv.URL, e2eSecret, sigHeader, idHeader, 3*time.Second)
	require.NoError(t, err)
	service := usecase.NewService(sender, nil)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	transport.NewServer(service, nil).Register(router)
	gatewaySrv := httptest.NewServer(router)
	defer gatewaySrv.Close()

	createBody := `{"payment_id":"` + e2ePaymentID + `","amount":{"minor_units":100000,"asset":"USDT","decimals":2},` +
		`"network":"trc20","expires_at":"2026-10-14T13:00:00Z"}`

	// Step 1: invoice creation and idempotent replay.
	status, payload := postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/invoices", createBody, map[string]string{"Idempotency-Key": "e2e-1"})
	require.Equal(t, http.StatusCreated, status)
	gatewayID := gatewayIDFrom(t, payload)

	status, payload = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/invoices", createBody, map[string]string{"Idempotency-Key": "e2e-1"})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, gatewayID, gatewayIDFrom(t, payload))

	// Step 2: confirmation with a duplicate: two accepted webhooks of one event,
	// the signature is valid, the replay is marked duplicate.
	status, payload = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","status":"confirmed","duplicate":2,"txid":"0xabc","confirmations":5}`, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(payload), `"http_status":200`)
	assert.Contains(t, string(payload), `"mode":"valid"`)

	records := recvr.recordsCopy()
	require.Len(t, records, 2)
	first, second := records[0], records[1]
	assert.True(t, first.verified, "the signature is valid")
	assert.Equal(t, first.eventID, second.eventID, "duplicates of one event")
	assert.Equal(t, first.body, second.body, "duplicates carry the same body")
	assert.False(t, first.duplicate)
	assert.True(t, second.duplicate, "the replay is recognized by receiver deduplication")
	assert.Equal(t, "confirmed", first.body.Status)
	assert.Equal(t, e2ePaymentID, first.body.PaymentID)
	assert.Equal(t, gatewayID, first.body.GatewayID)
	assert.Equal(t, int64(100_000), first.body.ReceivedAmount.MinorUnits)
	assert.Equal(t, "USDT", first.body.ReceivedAmount.Asset)
	assert.Equal(t, uint32(5), first.body.Confirmations)
	assert.Equal(t, "0xabc", first.body.TXID)
	assert.Equal(t, "trc20", first.body.Network)
	_, err = time.Parse(time.RFC3339Nano, first.body.EventTS)
	assert.NoError(t, err, "event_ts is in RFC 3339")

	// Step 3: a corrupted signature is rejected by the receiver.
	status, payload = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","status":"failed","signature":"invalid"}`, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(payload), `"http_status":401`)
	assert.Contains(t, string(payload), `"mode":"invalid"`)
	records = recvr.recordsCopy()
	require.Len(t, records, 3)
	assert.False(t, records[2].verified, "the corrupted signature is rejected")
	assert.Equal(t, uint32(0), records[2].body.Confirmations, "confirmations default to zero")
	assert.Empty(t, records[2].body.TXID, "txid is empty by default")

	// Step 4: a missing signature is rejected by the receiver.
	status, payload = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","status":"failed","signature":"missing"}`, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(payload), `"mode":"missing"`)
	records = recvr.recordsCopy()
	require.Len(t, records, 4)
	assert.False(t, records[3].verified, "the webhook without a signature is rejected")

	// Step 5: delay and ordering: confirmed is delivered immediately, detected -
	// with a delay and an earlier event_ts; the receiver sees events out of order.
	status, payload = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/invoices", createBody, nil)
	require.Equal(t, http.StatusCreated, status)
	orderGateway := gatewayIDFrom(t, payload)

	status, _ = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+orderGateway+`","status":"confirmed"}`, nil)
	require.Equal(t, http.StatusOK, status)

	earlyTS := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano)
	status, _ = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+orderGateway+`","status":"detected","delay_ms":300,"event_ts":"`+earlyTS+`"}`, nil)
	require.Equal(t, http.StatusOK, status)

	records = recvr.recordsCopy()
	require.Len(t, records, 6)
	confirmed, detected := records[4], records[5]
	assert.Equal(t, "confirmed", confirmed.body.Status)
	assert.Equal(t, "detected", detected.body.Status, "the event is delivered after a newer one (retry/race)")
	confirmedTS, err := time.Parse(time.RFC3339Nano, confirmed.body.EventTS)
	require.NoError(t, err)
	detectedTS, err := time.Parse(time.RFC3339Nano, detected.body.EventTS)
	require.NoError(t, err)
	assert.True(t, detectedTS.Before(confirmedTS), "event_ts reflects the order of provider events")

	// Step 6: provider API failure scenario.
	status, _ = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/scenarios", `{"fail_next":1}`, nil)
	require.Equal(t, http.StatusOK, status)

	status, payload = postJSON(t, http.MethodGet, gatewaySrv.URL+"/api/v1/invoices/"+gatewayID, "", nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	assert.Contains(t, string(payload), "provider_unavailable")

	status, _ = postJSON(t, http.MethodGet, gatewaySrv.URL+"/api/v1/invoices/"+gatewayID, "", nil)
	require.Equal(t, http.StatusOK, status)

	// The delivery log contains all deliveries, including rejected ones.
	status, payload = postJSON(t, http.MethodGet, gatewaySrv.URL+"/api/v1/webhooks", "", nil)
	require.Equal(t, http.StatusOK, status)
	var logResponse struct {
		Webhooks []struct {
			HTTPStatus int `json:"http_status"`
		} `json:"webhooks"`
	}
	require.NoError(t, json.Unmarshal(payload, &logResponse))
	require.Len(t, logResponse.Webhooks, 6)
	assert.Equal(t, 200, logResponse.Webhooks[0].HTTPStatus)
	assert.Equal(t, 401, logResponse.Webhooks[2].HTTPStatus)
	assert.Equal(t, 401, logResponse.Webhooks[3].HTTPStatus)
}
