//go:build e2e

// Package e2e contains the provider-profile scenario: each emulated provider
// signs its webhook with its own scheme and the sink verifies it with the kit
// webhooksig strategies, mirroring Webhook Ingress.
package e2e

import (
	"bytes"
	"encoding/json"
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

	"github.com/cbl/cbl-kit/webhooksig"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/transport"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
	"github.com/cbl/cbl-mock-gateway/internal/webhook"
)

const profileE2EPaymentID = "01a138fd-0000-7000-8000-000000000031"

// profileVerifier describes the sink verification of one profile.
type profileVerifier struct {
	strategy webhooksig.Strategy
	secret   []byte
}

// profileSink verifies provider-profile webhooks and records the results.
type profileSink struct {
	mu        sync.Mutex
	verifiers map[string]profileVerifier
	records   map[string][]profileRecord
}

// profileRecord describes one received profile webhook.
type profileRecord struct {
	verified bool
	eventID  string
	status   string
	body     []byte
}

// ServeHTTP verifies the webhook of the profile encoded in the first path segment.
func (s *profileSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	profile := strings.TrimPrefix(r.URL.Path, "/")
	verifier, ok := s.verifiers[profile]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	verified := verifier.strategy.Verify(verifier.secret, webhooksig.Request{Header: r.Header, Body: body}) == nil

	var statusValue string
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err == nil {
		for _, key := range []string{"status", "payment_status"} {
			if value, ok := decoded[key].(string); ok {
				statusValue = value
			}
		}
		if value, ok := decoded["type"].(string); ok {
			statusValue = strings.TrimPrefix(value, "Invoice")
		}
		if data, ok := decoded["data"].(map[string]any); ok {
			if value, ok := data["status"].(string); ok {
				statusValue = value
			}
		}
	}

	s.mu.Lock()
	s.records[profile] = append(s.records[profile], profileRecord{
		verified: verified,
		eventID:  r.Header.Get(idHeader),
		status:   statusValue,
		body:     body,
	})
	s.mu.Unlock()

	if !verified {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// recordsOf returns the records of a profile.
func (s *profileSink) recordsOf(profile string) []profileRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]profileRecord(nil), s.records[profile]...)
}

// strategies builds the sink verifiers from the profile signature specs.
func strategies() map[string]profileVerifier {
	return map[string]profileVerifier{
		"nowpayments": {secret: []byte("e2e-now-secret"),
			strategy: webhooksig.CanonicalJSONHMAC{Header: "x-nowpayments-sig", Algorithm: webhooksig.SHA512, Encoding: webhooksig.Hex}},
		"coingate": {secret: []byte("e2e-cg-token"),
			strategy: webhooksig.TokenEcho{Field: "token"}},
		"bitpay": {secret: []byte("e2e-bitpay-secret"),
			strategy: webhooksig.RawHMAC{Header: "x-signature", Algorithm: webhooksig.SHA256, Encoding: webhooksig.Base64}},
		"btcpay": {secret: []byte("e2e-btcpay-secret"),
			strategy: webhooksig.RawHMAC{Header: "BTCPay-Sig", Algorithm: webhooksig.SHA256, Encoding: webhooksig.Hex, Prefix: "sha256="}},
	}
}

// profileRoutes maps a profile to its create path and auth headers.
func profileRoutes() map[string]struct {
	path    string
	headers map[string]string
	body    string
	idField string
} {
	return map[string]struct {
		path    string
		headers map[string]string
		body    string
		idField string
	}{
		"nowpayments": {path: "/v1/invoice", headers: map[string]string{"x-api-key": "now-key"},
			body: `{"order_id":"` + profileE2EPaymentID + `","price_amount":12.34,"price_currency":"USDT","pay_currency":"usdttrc20"}`, idField: "id"},
		"coingate": {path: "/v2/orders", headers: map[string]string{"Authorization": "Bearer cg-token"},
			body: `{"order_id":"` + profileE2EPaymentID + `","price_amount":"12.34","price_currency":"USDT","pay_currency":"USDT_TRC20"}`, idField: "id"},
		"bitpay": {path: "/invoices", headers: map[string]string{"X-Identity": "id-1", "X-Signature": "c2ln"},
			body: `{"token":"tok","price":"12.34","currency":"USDT","orderId":"` + profileE2EPaymentID + `"}`, idField: "id"},
		"btcpay": {path: "/api/v1/stores/store-1/invoices", headers: map[string]string{"Authorization": "token btc-key"},
			body: `{"amount":"12.34","currency":"USDT","metadata":{"orderId":"` + profileE2EPaymentID + `","orderNetwork":"trc20"}}`, idField: "id"},
	}
}

// expectedRawStatus maps a profile to the raw status of the canonical "confirmed" simulate.
func expectedRawStatus(profile string) string {
	switch profile {
	case "nowpayments":
		return "finished"
	case "coingate":
		return "paid"
	case "bitpay":
		return "complete"
	default:
		return "Settled"
	}
}

// profileResponse describes a raw profile request result.
type profileResponse struct {
	code    int
	headers http.Header
	payload []byte
}

// postProfileJSON performs a JSON request and returns the full response.
func postProfileJSON(t *testing.T, method, url, body string, headers map[string]string) profileResponse {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
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
	return profileResponse{code: response.StatusCode, headers: response.Header, payload: payload}
}

// gatewayIDFromProfile extracts the gateway id (string or number) from a response.
func gatewayIDFromProfile(t *testing.T, payload []byte) string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	if response["facade"] == "merchant/invoice" {
		// The real API wraps invoice bodies in the facade envelope (17.8).
		if data, ok := response["data"].(map[string]any); ok {
			response = data
		}
	}
	switch id := response["id"].(type) {
	case string:
		require.NotEmpty(t, id)
		return id
	case json.Number:
		return id.String()
	default:
		require.FailNow(t, "response has no provider id", string(payload))
		return ""
	}
}

func TestE2EMockGatewayProviderProfiles(t *testing.T) {
	recvr := &profileSink{verifiers: strategies(), records: map[string][]profileRecord{}}
	sinkSrv := httptest.NewServer(recvr)
	defer sinkSrv.Close()

	senders := map[domain.ProfileID]usecase.WebhookSender{}
	legacy, err := webhook.NewHTTPSender(sinkSrv.URL+"/mock", e2eSecret, sigHeader, idHeader, 3*time.Second)
	require.NoError(t, err)
	senders[domain.ProfileMock] = legacy
	for _, id := range []domain.ProfileID{domain.ProfileNOWPayments, domain.ProfileCoinGate, domain.ProfileBitPay, domain.ProfileBTCPay} {
		profile, ok := domain.ProfileByID(string(id))
		require.True(t, ok)
		secret := strategies()[string(id)].secret
		sender, err := webhook.NewProfileSender(profile, sinkSrv.URL+"/"+string(id), secret, idHeader, 3*time.Second)
		require.NoError(t, err)
		senders[id] = sender
	}
	switcher, err := webhook.NewSwitch(senders)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	service := usecase.NewService(switcher, nil, usecase.WithTargets(switcher))
	transport.NewServer(service, nil).Register(router)
	gatewaySrv := httptest.NewServer(router)
	defer gatewaySrv.Close()

	for profile, route := range profileRoutes() {
		t.Run(profile, func(t *testing.T) {
			unauthorized := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+route.path, route.body, nil)
			require.Equal(t, http.StatusUnauthorized, unauthorized.code)

			created := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+route.path, route.body, route.headers)
			require.Equal(t, http.StatusCreated, created.code, string(created.payload))
			gatewayID := gatewayIDFromProfile(t, created.payload)

			replayed := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+route.path, route.body, route.headers)
			require.Equal(t, http.StatusOK, replayed.code)
			assert.Equal(t, gatewayID, gatewayIDFromProfile(t, replayed.payload))

			simulateBody := `{"gateway_id":"` + gatewayID + `","provider":"` + profile + `","status":"confirmed"}`
			simulate := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate", simulateBody, nil)
			require.Equal(t, http.StatusOK, simulate.code, string(simulate.payload))

			records := recvr.recordsOf(profile)
			require.Len(t, records, 1)
			assert.True(t, records[0].verified, "valid signature must verify")
			assert.NotEmpty(t, records[0].eventID)
			assert.Equal(t, expectedRawStatus(profile), records[0].status)

			simulate = postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
				`{"gateway_id":"`+gatewayID+`","provider":"`+profile+`","status":"confirmed","signature":"invalid"}`, nil)
			require.Equal(t, http.StatusOK, simulate.code)
			records = recvr.recordsOf(profile)
			require.Len(t, records, 2)
			assert.False(t, records[1].verified, "corrupted signature must not verify")

			simulate = postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
				`{"gateway_id":"`+gatewayID+`","provider":"`+profile+`","status":"confirmed","signature":"missing"}`, nil)
			require.Equal(t, http.StatusOK, simulate.code)
			records = recvr.recordsOf(profile)
			require.Len(t, records, 3)
			assert.False(t, records[2].verified, "missing signature must not verify")
		})
	}

	t.Run("injected rate limit", func(t *testing.T) {
		scenario := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/scenarios", `{"fail_next":1,"fail_status":429,"retry_after_s":30}`, nil)
		require.Equal(t, http.StatusOK, scenario.code)
		route := profileRoutes()["coingate"]
		limited := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+route.path, route.body, route.headers)
		require.Equal(t, http.StatusTooManyRequests, limited.code)
		assert.Equal(t, "30", limited.headers.Get("Retry-After"))
	})
}
