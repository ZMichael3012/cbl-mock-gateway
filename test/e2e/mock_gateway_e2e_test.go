//go:build e2e

// Пакет e2e содержит сквозной сценарий mock-gateway: создание счета,
// подписанные вебхуки (валидные, дубликаты, порченные), порядок и сценарии
// сбоев. Получатель проверяет подпись kit hmac и дедуплицирует события так же,
// как Webhook Ingress.
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

	"github.com/cbl/cbl-kit/hmac"

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

// sandboxBody описывает тело вебхука по контракту sandbox-провайдера.
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

// sinkRecord описывает принятый вебхук.
type sinkRecord struct {
	eventID   string
	verified  bool
	duplicate bool
	body      sandboxBody
}

// sink принимает вебхуки: проверяет подпись и дедуплицирует по event_id.
type sink struct {
	mu      sync.Mutex
	secret  []byte
	records []sinkRecord
	seen    map[string]bool
}

// ServeHTTP реализует прием вебхука.
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

// recordsCopy возвращает копию принятых записей.
func (s *sink) recordsCopy() []sinkRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkRecord(nil), s.records...)
}

// postJSON выполняет JSON-запрос к стенду.
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

// gatewayIDFrom извлекает gateway_id из ответа.
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

	// Шаг 1: создание счета и идемпотентный повтор.
	status, payload := postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/invoices", createBody, map[string]string{"Idempotency-Key": "e2e-1"})
	require.Equal(t, http.StatusCreated, status)
	gatewayID := gatewayIDFrom(t, payload)

	status, payload = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/invoices", createBody, map[string]string{"Idempotency-Key": "e2e-1"})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, gatewayID, gatewayIDFrom(t, payload))

	// Шаг 2: подтверждение с дубликатом: два принятых вебхука одного события,
	// подпись валидна, повтор помечен duplicate.
	status, payload = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","status":"confirmed","duplicate":2,"txid":"0xabc","confirmations":5}`, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(payload), `"http_status":200`)
	assert.Contains(t, string(payload), `"mode":"valid"`)

	records := recvr.recordsCopy()
	require.Len(t, records, 2)
	first, second := records[0], records[1]
	assert.True(t, first.verified, "подпись валидна")
	assert.Equal(t, first.eventID, second.eventID, "дубликаты одного события")
	assert.Equal(t, first.body, second.body, "дубликаты несут одно тело")
	assert.False(t, first.duplicate)
	assert.True(t, second.duplicate, "повтор распознан дедупликацией получателя")
	assert.Equal(t, "confirmed", first.body.Status)
	assert.Equal(t, e2ePaymentID, first.body.PaymentID)
	assert.Equal(t, gatewayID, first.body.GatewayID)
	assert.Equal(t, int64(100_000), first.body.ReceivedAmount.MinorUnits)
	assert.Equal(t, "USDT", first.body.ReceivedAmount.Asset)
	assert.Equal(t, uint32(5), first.body.Confirmations)
	assert.Equal(t, "0xabc", first.body.TXID)
	assert.Equal(t, "trc20", first.body.Network)
	_, err = time.Parse(time.RFC3339Nano, first.body.EventTS)
	assert.NoError(t, err, "event_ts в RFC 3339")

	// Шаг 3: испорченная подпись отвергается получателем.
	status, payload = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","status":"failed","signature":"invalid"}`, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(payload), `"http_status":401`)
	assert.Contains(t, string(payload), `"mode":"invalid"`)
	records = recvr.recordsCopy()
	require.Len(t, records, 3)
	assert.False(t, records[2].verified, "порченная подпись отвергнута")
	assert.Equal(t, uint32(0), records[2].body.Confirmations, "подтверждения по умолчанию нулевые")
	assert.Empty(t, records[2].body.TXID, "txid по умолчанию пуст")

	// Шаг 4: отсутствующая подпись отвергается получателем.
	status, payload = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","status":"failed","signature":"missing"}`, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(payload), `"mode":"missing"`)
	records = recvr.recordsCopy()
	require.Len(t, records, 4)
	assert.False(t, records[3].verified, "без подписи вебхук отвергнут")

	// Шаг 5: задержка и порядок: confirmed уходит сразу, detected - с задержкой
	// и более ранним event_ts; получатель видит события не по порядку.
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
	assert.Equal(t, "detected", detected.body.Status, "событие доставлено после более нового (retry/гонка)")
	confirmedTS, err := time.Parse(time.RFC3339Nano, confirmed.body.EventTS)
	require.NoError(t, err)
	detectedTS, err := time.Parse(time.RFC3339Nano, detected.body.EventTS)
	require.NoError(t, err)
	assert.True(t, detectedTS.Before(confirmedTS), "event_ts отражает порядок событий провайдера")

	// Шаг 6: сценарий сбоя API провайдера.
	status, _ = postJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/scenarios", `{"fail_next":1}`, nil)
	require.Equal(t, http.StatusOK, status)

	status, payload = postJSON(t, http.MethodGet, gatewaySrv.URL+"/api/v1/invoices/"+gatewayID, "", nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	assert.Contains(t, string(payload), "provider_unavailable")

	status, _ = postJSON(t, http.MethodGet, gatewaySrv.URL+"/api/v1/invoices/"+gatewayID, "", nil)
	require.Equal(t, http.StatusOK, status)

	// Журнал отправок содержит все доставки, включая отвергнутые.
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
