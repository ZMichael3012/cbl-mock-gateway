package transport_test

import (
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

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/transport"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
)

// profileOrderID sets the order reference for profile transport tests.
const profileOrderID = "01a138fd-0000-7000-8000-000000000021"

// capturingSender records webhook events for assertions.
type capturingSender struct {
	mu     sync.Mutex
	events []usecase.WebhookEvent
}

// Send records the event and returns a successful delivery.
func (s *capturingSender) Send(_ context.Context, event usecase.WebhookEvent) usecase.Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return usecase.Delivery{HTTPStatus: 200, SentAt: time.Now().UTC()}
}

// lastEvent returns the last recorded event.
func (s *capturingSender) lastEvent(t *testing.T) usecase.WebhookEvent {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.events)
	return s.events[len(s.events)-1]
}

// staticTargets resolves webhook targets for the tests.
type staticTargets struct {
	missing bool
}

// HasTarget reports whether the profile has a target.
func (t staticTargets) HasTarget(domain.ProfileID) bool { return !t.missing }

// newProfileRouter builds a router with a capturing sender and target resolver.
func newProfileRouter(t *testing.T, missingTargets bool) (*gin.Engine, *capturingSender) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	sender := &capturingSender{}
	service := usecase.NewService(sender, nil, usecase.WithTargets(staticTargets{missing: missingTargets}))
	transport.NewServer(service, nil).Register(router)
	return router, sender
}

// decodeProfileResponse decodes a response body with UseNumber and unwraps the
// BitPay facade envelope (the real API wraps invoice bodies in facade/data).
func decodeProfileResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(recorder.Body.String()))
	decoder.UseNumber()
	var decoded map[string]any
	require.NoError(t, decoder.Decode(&decoded))
	if decoded["facade"] == "merchant/invoice" {
		if data, ok := decoded["data"].(map[string]any); ok {
			return data
		}
	}
	return decoded
}

// orderBody builds a provider-shaped create body for the profile.
func orderBody(profile string, orderID string) string {
	switch profile {
	case "nowpayments":
		return `{"order_id":"` + orderID + `","price_amount":12.34,"price_currency":"USDT","pay_currency":"usdttrc20"}`
	case "coingate":
		return `{"order_id":"` + orderID + `","price_amount":"12.34","price_currency":"USDT","pay_currency":"USDT_TRC20"}`
	case "bitpay":
		return `{"token":"tok","price":"12.34","currency":"USDT","orderId":"` + orderID + `"}`
	default:
		return `{"amount":"12.34","currency":"USDT","metadata":{"orderId":"` + orderID + `","orderNetwork":"trc20"}}`
	}
}

// createPath and getPath return the provider routes.
func createPath(profile string) string {
	switch profile {
	case "nowpayments":
		return "/v1/invoice"
	case "coingate":
		return "/v2/orders"
	case "bitpay":
		return "/invoices"
	default:
		return "/api/v1/stores/store-1/invoices"
	}
}

// authHeaders returns the valid auth headers of a profile.
func authHeaders(profile string) map[string]string {
	switch profile {
	case "nowpayments":
		return map[string]string{"x-api-key": "now-key"}
	case "coingate":
		return map[string]string{"Authorization": "Bearer cg-token"}
	case "bitpay":
		return map[string]string{"X-Identity": "id-1", "X-Signature": "c2ln"}
	default:
		return map[string]string{"Authorization": "token btc-key"}
	}
}

// createProfileOrder creates an invoice and returns the gateway id.
func createProfileOrder(t *testing.T, router *gin.Engine, profile string) string {
	t.Helper()
	recorder := doJSON(router, http.MethodPost, createPath(profile), orderBody(profile, profileOrderID), authHeaders(profile))
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	id, _ := decodeProfileResponse(t, recorder)["id"].(string)
	if id == "" {
		if number, ok := decodeProfileResponse(t, recorder)["id"].(json.Number); ok {
			id = number.String()
		}
	}
	require.NotEmpty(t, id)
	return id
}

func TestProfileAuthMatrix(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	for _, profile := range []string{"nowpayments", "coingate", "bitpay", "btcpay"} {
		t.Run(profile, func(t *testing.T) {
			missing := doJSON(router, http.MethodPost, createPath(profile), orderBody(profile, profileOrderID), nil)
			assert.Equal(t, http.StatusUnauthorized, missing.Code)
			assert.Contains(t, missing.Header().Get("Content-Type"), "problem+json")

			empty := doJSON(router, http.MethodPost, createPath(profile), orderBody(profile, profileOrderID),
				map[string]string{"x-api-key": ""})
			if profile != "nowpayments" {
				empty = doJSON(router, http.MethodPost, createPath(profile), orderBody(profile, profileOrderID),
					map[string]string{"Authorization": ""})
			}
			assert.Equal(t, http.StatusUnauthorized, empty.Code)

			valid := doJSON(router, http.MethodPost, createPath(profile), orderBody(profile, profileOrderID), authHeaders(profile))
			require.Equal(t, http.StatusCreated, valid.Code,
				"a 401 attempt must not register the order: a registered order would replay with 200")
		})
	}

	wrongPrefix := doJSON(router, http.MethodPost, "/v2/orders", orderBody("coingate", profileOrderID),
		map[string]string{"Authorization": "Basic abc"})
	assert.Equal(t, http.StatusUnauthorized, wrongPrefix.Code)

	missingBitPaySig := doJSON(router, http.MethodPost, "/invoices", orderBody("bitpay", profileOrderID),
		map[string]string{"X-Identity": "02abcd"})
	assert.Equal(t, http.StatusUnauthorized, missingBitPaySig.Code)
}

func TestProfileNOWPaymentsRoundTrip(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	recorder := doJSON(router, http.MethodPost, "/v1/invoice", orderBody("nowpayments", profileOrderID), authHeaders("nowpayments"))
	require.Equal(t, http.StatusCreated, recorder.Code)
	decoded := decodeProfileResponse(t, recorder)
	assert.Equal(t, profileOrderID, decoded["order_id"])
	assert.Equal(t, json.Number("12.340000"), decoded["price_amount"])
	assert.Equal(t, "USDT", decoded["price_currency"])
	assert.Equal(t, "usdttrc20", decoded["pay_currency"])
	assert.NotEmpty(t, decoded["invoice_url"])
	gatewayID, _ := decoded["id"].(string)
	require.NotEmpty(t, gatewayID)

	get := doJSON(router, http.MethodGet, "/v1/payment/"+gatewayID, "", authHeaders("nowpayments"))
	require.Equal(t, http.StatusOK, get.Code)
	assert.Equal(t, "waiting", decodeProfileResponse(t, get)["payment_status"])

	simulate := doJSON(router, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","provider":"nowpayments","status":"detected"}`, nil)
	require.Equal(t, http.StatusOK, simulate.Code)

	get = doJSON(router, http.MethodGet, "/v1/payment/"+gatewayID, "", authHeaders("nowpayments"))
	require.Equal(t, http.StatusOK, get.Code)
	payment := decodeProfileResponse(t, get)
	assert.Equal(t, "confirming", payment["payment_status"])
	assert.Equal(t, profileOrderID, payment["order_id"])
}

func TestProfileCoinGateRoundTrip(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	recorder := doJSON(router, http.MethodPost, "/v2/orders", orderBody("coingate", profileOrderID), authHeaders("coingate"))
	require.Equal(t, http.StatusCreated, recorder.Code)
	decoded := decodeProfileResponse(t, recorder)
	id, ok := decoded["id"].(json.Number)
	require.True(t, ok, "coingate id must be a JSON number, got %T", decoded["id"])
	assert.Equal(t, "new", decoded["status"])
	assert.Equal(t, "12.340000", decoded["price_amount"])
	assert.NotEmpty(t, decoded["payment_address"])

	get := doJSON(router, http.MethodGet, "/v2/orders/"+id.String(), "", authHeaders("coingate"))
	require.Equal(t, http.StatusOK, get.Code)
	assert.Equal(t, "new", decodeProfileResponse(t, get)["status"])
}

func TestProfileBitPayRoundTrip(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	recorder := doJSON(router, http.MethodPost, "/invoices", orderBody("bitpay", profileOrderID), authHeaders("bitpay"))
	require.Equal(t, http.StatusCreated, recorder.Code)
	decoded := decodeProfileResponse(t, recorder)
	gatewayID, ok := decoded["id"].(string)
	require.True(t, ok)
	assert.Len(t, gatewayID, 22)
	assert.Equal(t, "new", decoded["status"])
	assert.Equal(t, json.Number("12.340000"), decoded["price"])
	assert.NotEmpty(t, decoded["url"])
	_, isNumber := decoded["invoiceTime"].(json.Number)
	assert.True(t, isNumber)

	get := doJSON(router, http.MethodGet, "/invoices/"+gatewayID, "", authHeaders("bitpay"))
	require.Equal(t, http.StatusOK, get.Code)
	assert.Equal(t, "new", decodeProfileResponse(t, get)["status"])
}

func TestProfileBTCPayRoundTrip(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	recorder := doJSON(router, http.MethodPost, "/api/v1/stores/store-1/invoices", orderBody("btcpay", profileOrderID), authHeaders("btcpay"))
	require.Equal(t, http.StatusCreated, recorder.Code)
	decoded := decodeProfileResponse(t, recorder)
	gatewayID, ok := decoded["id"].(string)
	require.True(t, ok)
	assert.Len(t, gatewayID, 32)
	assert.Equal(t, "New", decoded["status"])
	assert.Equal(t, "12.340000", decoded["amount"])
	assert.NotEmpty(t, decoded["checkoutLink"])

	get := doJSON(router, http.MethodGet, "/api/v1/stores/store-1/invoices/"+gatewayID, "", authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, get.Code)
	assert.Equal(t, "New", decodeProfileResponse(t, get)["status"])
}

func TestProfileReplayIsolation(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	first := createProfileOrder(t, router, "coingate")

	replay := doJSON(router, http.MethodPost, "/v2/orders", orderBody("coingate", profileOrderID), authHeaders("coingate"))
	require.Equal(t, http.StatusOK, replay.Code)
	replayedID := decodeProfileResponse(t, replay)["id"].(json.Number).String()
	assert.Equal(t, first, replayedID)

	other := createProfileOrder(t, router, "nowpayments")
	assert.NotEqual(t, first, other)
}

func TestProfileCrossProviderAccessRejected(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	coinGateID := createProfileOrder(t, router, "coingate")

	foreignGet := doJSON(router, http.MethodGet, "/v1/payment/"+coinGateID, "", authHeaders("nowpayments"))
	require.Equal(t, http.StatusNotFound, foreignGet.Code, "a foreign invoice id must not render a NOWPayments body")

	legacyGet := doJSON(router, http.MethodGet, "/api/v1/invoices/"+coinGateID, "", nil)
	require.Equal(t, http.StatusNotFound, legacyGet.Code, "the legacy route must not render the mock body for a profile invoice")

	foreignSimulate := doJSON(router, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+coinGateID+`","provider":"nowpayments","status":"confirmed"}`, nil)
	require.Equal(t, http.StatusBadRequest, foreignSimulate.Code)
	assert.Contains(t, foreignSimulate.Body.String(), "provider_mismatch")
}

func TestScenarioInjectedFailStatus(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	scenario := doJSON(router, http.MethodPost, "/api/v1/scenarios", `{"fail_next":1,"fail_status":429,"retry_after_s":30}`, nil)
	require.Equal(t, http.StatusOK, scenario.Code)
	decoded := decodeProfileResponse(t, scenario)
	assert.Equal(t, json.Number("429"), decoded["fail_status"])
	assert.Equal(t, json.Number("30"), decoded["retry_after_s"])

	limited := doJSON(router, http.MethodPost, "/v1/invoice", orderBody("nowpayments", profileOrderID), authHeaders("nowpayments"))
	require.Equal(t, http.StatusTooManyRequests, limited.Code)
	assert.Equal(t, "30", limited.Header().Get("Retry-After"))
	assert.Contains(t, limited.Header().Get("Content-Type"), "problem+json")

	after := doJSON(router, http.MethodPost, "/v1/invoice", orderBody("nowpayments", profileOrderID), authHeaders("nowpayments"))
	require.Equal(t, http.StatusCreated, after.Code)

	scenario = doJSON(router, http.MethodPost, "/api/v1/scenarios", `{"fail_next":1}`, nil)
	require.Equal(t, http.StatusOK, scenario.Code)
	unavailable := doJSON(router, http.MethodPost, "/v1/invoice", orderBody("nowpayments", profileOrderID), authHeaders("nowpayments"))
	require.Equal(t, http.StatusServiceUnavailable, unavailable.Code)
	assert.Empty(t, unavailable.Header().Get("Retry-After"))
}

func TestSimulateProviderRouting(t *testing.T) {
	router, sender := newProfileRouter(t, false)
	gatewayID := createProfileOrder(t, router, "coingate")

	simulate := doJSON(router, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","provider":"coingate","status":"confirmed"}`, nil)
	require.Equal(t, http.StatusOK, simulate.Code)
	decoded := decodeProfileResponse(t, simulate)
	deliveries, ok := decoded["deliveries"].([]any)
	require.True(t, ok)
	require.Len(t, deliveries, 1)
	assert.Equal(t, "coingate", deliveries[0].(map[string]any)["provider"])

	event := sender.lastEvent(t)
	assert.Equal(t, domain.ProfileCoinGate, event.Provider)
	assert.Equal(t, domain.StatusConfirmed, event.Status)
	assert.Equal(t, "paid", event.RawStatus)
}

func TestSimulateRawStatusOverride(t *testing.T) {
	router, sender := newProfileRouter(t, false)
	gatewayID := createProfileOrder(t, router, "nowpayments")

	simulate := doJSON(router, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","provider":"nowpayments","status":"confirmed","raw_status":"partially_paid"}`, nil)
	require.Equal(t, http.StatusOK, simulate.Code)
	event := sender.lastEvent(t)
	assert.Equal(t, domain.StatusConfirmed, event.Status)
	assert.Equal(t, "partially_paid", event.RawStatus)
}

func TestSimulateValidationErrors(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	gatewayID := createProfileOrder(t, router, "nowpayments")

	unknownProvider := doJSON(router, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","provider":"nope","status":"confirmed"}`, nil)
	assert.Equal(t, http.StatusBadRequest, unknownProvider.Code)

	unknownRaw := doJSON(router, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","provider":"nowpayments","status":"confirmed","raw_status":"nope"}`, nil)
	assert.Equal(t, http.StatusBadRequest, unknownRaw.Code)
	assert.Contains(t, unknownRaw.Body.String(), "unknown_raw_status")
}

func TestSimulateMissingTarget(t *testing.T) {
	router, _ := newProfileRouter(t, true)
	gatewayID := createProfileOrder(t, router, "nowpayments")

	simulate := doJSON(router, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","provider":"nowpayments","status":"confirmed"}`, nil)
	require.Equal(t, http.StatusBadRequest, simulate.Code)
	assert.Contains(t, simulate.Body.String(), "missing_webhook_target")
}

func TestDeliveryLogCarriesProvider(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	gatewayID := createProfileOrder(t, router, "bitpay")
	simulate := doJSON(router, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","provider":"bitpay","status":"confirmed"}`, nil)
	require.Equal(t, http.StatusOK, simulate.Code)

	log := doJSON(router, http.MethodGet, "/api/v1/webhooks", "", nil)
	require.Equal(t, http.StatusOK, log.Code)
	var response struct {
		Webhooks []struct {
			Provider string `json:"provider"`
			Status   string `json:"status"`
		} `json:"webhooks"`
	}
	require.NoError(t, json.Unmarshal(log.Body.Bytes(), &response))
	require.NotEmpty(t, response.Webhooks)
	assert.Equal(t, "bitpay", response.Webhooks[0].Provider)
}
