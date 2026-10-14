package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-mock-gateway/internal/transport"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
)

// testPaymentID задает UUID платежа для тестов транспорта.
const testPaymentID = "01a138fd-0000-7000-8000-000000000003"

// fakeSender запоминает вебхуки и возвращает успешную доставку.
type fakeSender struct {
	mu     sync.Mutex
	events int
}

// Send учитывает отправку и возвращает успех.
func (s *fakeSender) Send(_ context.Context, _ usecase.WebhookEvent) usecase.Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events++
	return usecase.Delivery{HTTPStatus: 200, SentAt: time.Now().UTC()}
}

// newRouter собирает маршрутизатор с реальным сервисом и fake-отправителем.
func newRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	server := transport.NewServer(usecase.NewService(&fakeSender{}, nil), nil)
	server.Register(router)
	return router
}

// doJSON выполняет запрос к роутеру.
func doJSON(router *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// createBody собирает тело создания счета.
func createBody() string {
	return `{"payment_id":"` + testPaymentID + `","amount":{"minor_units":100000,"asset":"USDT","decimals":2},` +
		`"network":"trc20","expires_at":"2026-10-14T13:00:00Z"}`
}

// decodeInvoice читает gateway_id из ответа.
func decodeInvoice(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var response struct {
		GatewayID string `json:"gateway_id"`
		Replayed  bool   `json:"replayed"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	return response.GatewayID
}

func TestHTTPCreateAndReplay(t *testing.T) {
	router := newRouter(t)

	first := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody(), map[string]string{"Idempotency-Key": "k1"})
	require.Equal(t, http.StatusCreated, first.Code)
	gatewayID := decodeInvoice(t, first)
	require.NotEmpty(t, gatewayID)

	second := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody(), map[string]string{"Idempotency-Key": "k1"})
	require.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, gatewayID, decodeInvoice(t, second))
	assert.Contains(t, second.Body.String(), `"replayed":true`)
}

func TestHTTPValidationErrors(t *testing.T) {
	router := newRouter(t)

	badJSON := doJSON(router, http.MethodPost, "/api/v1/invoices", `{broken`, nil)
	require.Equal(t, http.StatusBadRequest, badJSON.Code)
	assert.Contains(t, badJSON.Header().Get("Content-Type"), "application/problem+json")

	unknownField := doJSON(router, http.MethodPost, "/api/v1/invoices",
		`{"payment_id":"`+testPaymentID+`","amount":{"minor_units":1,"asset":"USDT","decimals":2},"network":"trc20","expires_at":"2026-10-14T13:00:00Z","surprise":1}`, nil)
	require.Equal(t, http.StatusBadRequest, unknownField.Code, "неизвестные поля запрещены")

	trailing := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody()+`{"extra":true}`, nil)
	require.Equal(t, http.StatusBadRequest, trailing.Code, "trailing json запрещен")

	badPayment := doJSON(router, http.MethodPost, "/api/v1/invoices",
		`{"payment_id":"pay-1","amount":{"minor_units":1,"asset":"USDT","decimals":2},"network":"trc20","expires_at":"2026-10-14T13:00:00Z"}`, nil)
	require.Equal(t, http.StatusBadRequest, badPayment.Code)

	badExpires := doJSON(router, http.MethodPost, "/api/v1/invoices",
		`{"payment_id":"`+testPaymentID+`","amount":{"minor_units":1,"asset":"USDT","decimals":2},"network":"trc20","expires_at":"yesterday"}`, nil)
	require.Equal(t, http.StatusBadRequest, badExpires.Code)
}

func TestHTTPBodyLimit(t *testing.T) {
	router := newRouter(t)

	huge := `{"payment_id":"` + testPaymentID + `","padding":"` + strings.Repeat("p", 65*(1<<10)) + `"}`
	recorder := doJSON(router, http.MethodPost, "/api/v1/invoices", huge, nil)
	require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "body_too_large")
}

func TestHTTPGetInvoice(t *testing.T) {
	router := newRouter(t)
	created := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody(), nil)
	gatewayID := decodeInvoice(t, created)

	found := doJSON(router, http.MethodGet, "/api/v1/invoices/"+gatewayID, "", nil)
	require.Equal(t, http.StatusOK, found.Code)
	assert.Contains(t, found.Body.String(), `"status":"pending"`)

	missing := doJSON(router, http.MethodGet, "/api/v1/invoices/unknown", "", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)
	assert.Contains(t, missing.Body.String(), "invoice_not_found")
}

func TestHTTPSimulate(t *testing.T) {
	router := newRouter(t)
	created := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody(), nil)
	gatewayID := decodeInvoice(t, created)

	simulated := doJSON(router, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","status":"completed","duplicate":2}`, nil)
	require.Equal(t, http.StatusOK, simulated.Code)
	assert.Contains(t, simulated.Body.String(), `"event_id"`)
	assert.Equal(t, 2, bytes.Count(simulated.Body.Bytes(), []byte(`"http_status":200`)))
	assert.Contains(t, simulated.Body.String(), `"mode":"valid"`)

	updated := doJSON(router, http.MethodGet, "/api/v1/invoices/"+gatewayID, "", nil)
	assert.Contains(t, updated.Body.String(), `"status":"confirmed"`)

	unknown := doJSON(router, http.MethodPost, "/api/v1/simulate", `{"gateway_id":"unknown","status":"confirmed"}`, nil)
	require.Equal(t, http.StatusNotFound, unknown.Code)

	badStatus := doJSON(router, http.MethodPost, "/api/v1/simulate", `{"gateway_id":"`+gatewayID+`","status":"weird"}`, nil)
	require.Equal(t, http.StatusBadRequest, badStatus.Code)
}

func TestHTTPScenarioInjection(t *testing.T) {
	router := newRouter(t)

	set := doJSON(router, http.MethodPost, "/api/v1/scenarios", `{"fail_next":1,"latency_ms":10}`, nil)
	require.Equal(t, http.StatusOK, set.Code)

	failed := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody(), nil)
	require.Equal(t, http.StatusServiceUnavailable, failed.Code)
	assert.Contains(t, failed.Body.String(), "provider_unavailable")

	recovered := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody(), nil)
	require.Equal(t, http.StatusCreated, recovered.Code)

	current := doJSON(router, http.MethodGet, "/api/v1/scenarios", "", nil)
	assert.Contains(t, current.Body.String(), `"fail_next":0`)

	reset := doJSON(router, http.MethodPost, "/api/v1/scenarios/reset", "", nil)
	require.Equal(t, http.StatusOK, reset.Code)
}

func TestHTTPWebhooksLog(t *testing.T) {
	router := newRouter(t)
	created := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody(), nil)
	gatewayID := decodeInvoice(t, created)
	doJSON(router, http.MethodPost, "/api/v1/simulate", `{"gateway_id":"`+gatewayID+`","status":"detected"}`, nil)

	log := doJSON(router, http.MethodGet, "/api/v1/webhooks", "", nil)
	require.Equal(t, http.StatusOK, log.Code)
	assert.Contains(t, log.Body.String(), gatewayID)
	assert.Contains(t, log.Body.String(), `"http_status":200`)
}
