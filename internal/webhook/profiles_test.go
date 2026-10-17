package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-kit/money"
	"github.com/cbl/cbl-kit/webhooksig"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
	"github.com/cbl/cbl-mock-gateway/internal/webhook"
)

const (
	profilePaymentID = "01a138fd-0000-7000-8000-000000000004"
	profileGatewayID = "01a138fd-0000-7000-8000-000000000005"
)

var profileEventTS = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func profileEvent(t *testing.T, provider domain.ProfileID, raw string) usecase.WebhookEvent {
	t.Helper()
	return usecase.WebhookEvent{
		EventID:        "01a138fd-0000-7000-8000-00000000000e",
		Provider:       provider,
		PaymentID:      profilePaymentID,
		GatewayID:      profileGatewayID,
		Status:         domain.StatusConfirmed,
		RawStatus:      raw,
		Amount:         money.MustParse("12.34", "USDT", 6),
		ReceivedAmount: money.MustParse("12.34", "USDT", 6),
		Confirmations:  2,
		TXID:           "0xtx",
		Network:        "usdt_trc20",
		EventTS:        profileEventTS,
		Mode:           domain.SignatureValid,
	}
}

func profileByName(t *testing.T, id string) domain.Profile {
	t.Helper()
	profile, ok := domain.ProfileByID(id)
	require.True(t, ok)
	return profile
}

func decodeProfileBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var decoded map[string]any
	require.NoError(t, decoder.Decode(&decoded))
	return decoded
}

func withMode(event usecase.WebhookEvent, mode domain.SignatureMode) usecase.WebhookEvent {
	event.Mode = mode
	return event
}

func TestProfileWebhookNOWPayments(t *testing.T) {
	profile := profileByName(t, "nowpayments")
	secret := []byte("now-secret")
	event := profileEvent(t, domain.ProfileNOWPayments, "finished")
	event.Mode = domain.SignatureValid

	body, header, value, err := webhook.ProfileWebhook(profile, secret, event)
	require.NoError(t, err)
	assert.Equal(t, "x-nowpayments-sig", header)
	assert.NotEmpty(t, value)

	decoded := decodeProfileBody(t, body)
	assert.Equal(t, profileGatewayID, decoded["payment_id"])
	assert.Equal(t, profilePaymentID, decoded["order_id"])
	assert.Equal(t, "finished", decoded["payment_status"])
	assert.Equal(t, json.Number("12.340000"), decoded["actually_paid"])
	assert.Equal(t, json.Number("12.340000"), decoded["price_amount"])
	assert.Equal(t, "2026-10-17T12:00:00Z", decoded["updated_at"])

	strategy := webhooksig.CanonicalJSONHMAC{Header: "x-nowpayments-sig", Algorithm: webhooksig.SHA512, Encoding: webhooksig.Hex}
	require.NoError(t, strategy.Verify(secret, webhooksig.Request{Header: http.Header{"X-Nowpayments-Sig": []string{value}}, Body: body}))
	require.Error(t, strategy.Verify([]byte("other"), webhooksig.Request{Header: http.Header{"X-Nowpayments-Sig": []string{value}}, Body: body}))

	_, _, invalidValue, err := webhook.ProfileWebhook(profile, secret, withMode(event, domain.SignatureInvalid))
	require.NoError(t, err)
	require.Error(t, strategy.Verify(secret, webhooksig.Request{Header: http.Header{"X-Nowpayments-Sig": []string{invalidValue}}, Body: body}))

	_, missingHeader, missingValue, err := webhook.ProfileWebhook(profile, secret, withMode(event, domain.SignatureMissing))
	require.NoError(t, err)
	assert.Empty(t, missingHeader)
	assert.Empty(t, missingValue)
}

func TestProfileWebhookCoinGate(t *testing.T) {
	profile := profileByName(t, "coingate")
	secret := []byte("cg-token")
	event := profileEvent(t, domain.ProfileCoinGate, "paid")
	event.GatewayID = "1042"

	body, header, value, err := webhook.ProfileWebhook(profile, secret, event)
	require.NoError(t, err)
	assert.Empty(t, header)
	assert.Empty(t, value)

	decoded := decodeProfileBody(t, body)
	assert.Equal(t, profilePaymentID, decoded["order_id"])
	assert.Equal(t, "paid", decoded["status"])
	assert.Equal(t, "cg-token", decoded["token"])
	assert.Equal(t, "12.340000", decoded["price_amount"])
	assert.Equal(t, json.Number("1042"), decoded["id"])

	strategy := webhooksig.TokenEcho{Field: "token"}
	require.NoError(t, strategy.Verify(secret, webhooksig.Request{Body: body}))
	require.Error(t, strategy.Verify([]byte("wrong"), webhooksig.Request{Body: body}))

	invalidBody, _, _, err := webhook.ProfileWebhook(profile, secret, withMode(event, domain.SignatureInvalid))
	require.NoError(t, err)
	assert.NotEqual(t, secret, decodeProfileBody(t, invalidBody)["token"])

	missingBody, _, _, err := webhook.ProfileWebhook(profile, secret, withMode(event, domain.SignatureMissing))
	require.NoError(t, err)
	_, hasToken := decodeProfileBody(t, missingBody)["token"]
	assert.False(t, hasToken)
	require.Error(t, strategy.Verify(secret, webhooksig.Request{Body: missingBody}))
}

func TestProfileWebhookBitPay(t *testing.T) {
	profile := profileByName(t, "bitpay")
	secret := []byte("bitpay-secret")
	event := profileEvent(t, domain.ProfileBitPay, "complete")

	body, header, value, err := webhook.ProfileWebhook(profile, secret, event)
	require.NoError(t, err)
	assert.Equal(t, "x-signature", header)
	assert.NotEmpty(t, value)

	decoded := decodeProfileBody(t, body)
	data, ok := decoded["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, profileGatewayID, data["id"])
	assert.Equal(t, profilePaymentID, data["orderId"])
	assert.Equal(t, "complete", data["status"])
	eventBlock, ok := decoded["event"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "invoice_completed", eventBlock["name"])
	_, isNumber := data["invoiceTime"].(json.Number)
	assert.True(t, isNumber)

	strategy := webhooksig.RawHMAC{Header: "x-signature", Algorithm: webhooksig.SHA256, Encoding: webhooksig.Base64}
	require.NoError(t, strategy.Verify(secret, webhooksig.Request{Header: http.Header{"X-Signature": []string{value}}, Body: body}))
	require.Error(t, strategy.Verify([]byte("wrong"), webhooksig.Request{Header: http.Header{"X-Signature": []string{value}}, Body: body}))

	_, _, invalidValue, err := webhook.ProfileWebhook(profile, secret, withMode(event, domain.SignatureInvalid))
	require.NoError(t, err)
	require.Error(t, strategy.Verify(secret, webhooksig.Request{Header: http.Header{"X-Signature": []string{invalidValue}}, Body: body}))

	_, missingHeader, missingValue, err := webhook.ProfileWebhook(profile, secret, withMode(event, domain.SignatureMissing))
	require.NoError(t, err)
	assert.Empty(t, missingHeader)
	assert.Empty(t, missingValue)
}

func TestProfileWebhookBTCPay(t *testing.T) {
	profile := profileByName(t, "btcpay")
	secret := []byte("btcpay-secret")
	event := profileEvent(t, domain.ProfileBTCPay, "Settled")

	body, header, value, err := webhook.ProfileWebhook(profile, secret, event)
	require.NoError(t, err)
	assert.Equal(t, "BTCPay-Sig", header)
	assert.True(t, len(value) > len("sha256="))
	assert.Equal(t, "sha256=", value[:len("sha256=")])

	decoded := decodeProfileBody(t, body)
	assert.Equal(t, "InvoiceSettled", decoded["type"])
	assert.Equal(t, profileGatewayID, decoded["invoiceId"])
	assert.NotEmpty(t, decoded["deliveryId"])

	strategy := webhooksig.RawHMAC{Header: "BTCPay-Sig", Algorithm: webhooksig.SHA256, Encoding: webhooksig.Hex, Prefix: "sha256="}
	require.NoError(t, strategy.Verify(secret, webhooksig.Request{Header: http.Header{"Btcpay-Sig": []string{value}}, Body: body}))
	require.Error(t, strategy.Verify(secret, webhooksig.Request{Header: http.Header{"Btcpay-Sig": []string{value}}, Body: []byte("{}")}))
}

func TestProfileWebhookStatusMappings(t *testing.T) {
	bitpay := profileByName(t, "bitpay")
	secret := []byte("s")
	tests := map[string]string{
		"new":      "invoice_created",
		"paid":     "invoice_paidInFull",
		"complete": "invoice_completed",
		"expired":  "invoice_expired",
		"invalid":  "invoice_failedToConfirm",
	}
	for raw, name := range tests {
		body, _, _, err := webhook.ProfileWebhook(bitpay, secret, profileEvent(t, domain.ProfileBitPay, raw))
		require.NoError(t, err)
		decoded := decodeProfileBody(t, body)
		assert.Equal(t, name, decoded["event"].(map[string]any)["name"])
	}

	btcpay := profileByName(t, "btcpay")
	types := map[string]string{
		"New":        "InvoiceCreated",
		"Processing": "InvoiceProcessing",
		"Settled":    "InvoiceSettled",
		"Expired":    "InvoiceExpired",
		"Invalid":    "InvoiceInvalid",
	}
	for raw, name := range types {
		body, _, _, err := webhook.ProfileWebhook(btcpay, secret, profileEvent(t, domain.ProfileBTCPay, raw))
		require.NoError(t, err)
		decoded := decodeProfileBody(t, body)
		assert.Equal(t, name, decoded["type"])
	}
}

func TestNewProfileSenderValidation(t *testing.T) {
	profile := profileByName(t, "coingate")
	_, err := webhook.NewProfileSender(profile, "http://example.test/hooks", []byte("s"), "X-Event-Id", time.Second)
	require.NoError(t, err)

	legacy := profileByName(t, "mock")
	_, err = webhook.NewProfileSender(legacy, "http://example.test/hooks", []byte("s"), "X-Event-Id", time.Second)
	require.Error(t, err)

	unknown := domain.Profile{ID: "custom"}
	_, err = webhook.NewProfileSender(unknown, "http://example.test/hooks", []byte("s"), "X-Event-Id", time.Second)
	require.Error(t, err)

	headerless := profileByName(t, "bitpay")
	headerless.Signature.Header = ""
	_, err = webhook.NewProfileSender(headerless, "http://example.test/hooks", []byte("s"), "X-Event-Id", time.Second)
	require.Error(t, err)

	fieldless := profileByName(t, "coingate")
	fieldless.Signature.Field = ""
	_, err = webhook.NewProfileSender(fieldless, "http://example.test/hooks", []byte("s"), "X-Event-Id", time.Second)
	require.Error(t, err)

	_, err = webhook.NewProfileSender(profile, "", []byte("s"), "X-Event-Id", time.Second)
	require.Error(t, err)
	_, err = webhook.NewProfileSender(profile, "ftp://example.test/hooks", []byte("s"), "X-Event-Id", time.Second)
	require.Error(t, err)
	_, err = webhook.NewProfileSender(profile, "http://example.test/hooks", nil, "X-Event-Id", time.Second)
	require.Error(t, err)
	_, err = webhook.NewProfileSender(profile, "http://example.test/hooks", []byte("s"), " ", time.Second)
	require.Error(t, err)
}

func TestProfileWebhookFailsClosed(t *testing.T) {
	unknown := domain.Profile{ID: "custom"}
	_, _, _, err := webhook.ProfileWebhook(unknown, []byte("secret"), profileEvent(t, domain.ProfileCoinGate, "paid"))
	require.Error(t, err, "an unknown signature kind must fail closed")

	_, _, _, err = webhook.ProfileWebhook(profileByName(t, "coingate"), nil, profileEvent(t, domain.ProfileCoinGate, "paid"))
	require.Error(t, err, "an empty secret must fail closed")
}

func TestProfileSenderSend(t *testing.T) {
	profile := profileByName(t, "nowpayments")
	secret := []byte("now-secret")
	var gotBody []byte
	var gotHeader string
	sink := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		gotHeader = r.Header.Get("x-nowpayments-sig")
		assert.Equal(t, "01a138fd-0000-7000-8000-00000000000e", r.Header.Get("X-Event-Id"))
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(sink)
	defer server.Close()

	sender, err := webhook.NewProfileSender(profile, server.URL, secret, "X-Event-Id", time.Second)
	require.NoError(t, err)
	event := profileEvent(t, domain.ProfileNOWPayments, "finished")
	delivery := sender.Send(context.Background(), event)
	assert.Equal(t, http.StatusOK, delivery.HTTPStatus)
	assert.Equal(t, domain.ProfileNOWPayments, delivery.Provider)

	strategy := webhooksig.CanonicalJSONHMAC{Header: "x-nowpayments-sig", Algorithm: webhooksig.SHA512, Encoding: webhooksig.Hex}
	require.NoError(t, strategy.Verify(secret, webhooksig.Request{Header: http.Header{"X-Nowpayments-Sig": []string{gotHeader}}, Body: gotBody}))

	// A receiver error is recorded in the delivery, not returned as an error.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer failing.Close()
	sender, err = webhook.NewProfileSender(profile, failing.URL, secret, "X-Event-Id", time.Second)
	require.NoError(t, err)
	delivery = sender.Send(context.Background(), event)
	assert.Equal(t, http.StatusUnauthorized, delivery.HTTPStatus)
	assert.NotEmpty(t, delivery.Error)
}

func TestSwitch(t *testing.T) {
	_, err := webhook.NewSwitch(nil)
	require.Error(t, err)

	_, err = webhook.NewSwitch(map[domain.ProfileID]usecase.WebhookSender{"unknown": profileTestSender{}})
	require.Error(t, err)

	_, err = webhook.NewSwitch(map[domain.ProfileID]usecase.WebhookSender{domain.ProfileCoinGate: nil})
	require.Error(t, err)

	switcher, err := webhook.NewSwitch(map[domain.ProfileID]usecase.WebhookSender{domain.ProfileCoinGate: profileTestSender{}})
	require.NoError(t, err)
	assert.True(t, switcher.HasTarget(domain.ProfileCoinGate))
	assert.False(t, switcher.HasTarget(domain.ProfileBitPay))

	event := profileEvent(t, domain.ProfileCoinGate, "paid")
	delivery := switcher.Send(context.Background(), event)
	assert.Equal(t, domain.ProfileCoinGate, delivery.Provider)

	delivery = switcher.Send(context.Background(), profileEvent(t, domain.ProfileBitPay, "new"))
	assert.NotEmpty(t, delivery.Error)
}

// profileTestSender records nothing and answers successfully.
type profileTestSender struct{}

// Send returns a successful delivery.
func (profileTestSender) Send(_ context.Context, event usecase.WebhookEvent) usecase.Delivery {
	return usecase.Delivery{HTTPStatus: 200, Provider: event.Provider, SentAt: time.Now().UTC()}
}

func TestDeriveTarget(t *testing.T) {
	target, ok := webhook.DeriveTarget("http://localhost:8060/hooks/mock/ep1", domain.ProfileCoinGate)
	require.True(t, ok)
	assert.Equal(t, "http://localhost:8060/hooks/coingate/ep1", target)

	target, ok = webhook.DeriveTarget("http://localhost:8060/hooks/mock/ep1", domain.ProfileMock)
	require.True(t, ok)
	assert.Equal(t, "http://localhost:8060/hooks/mock/ep1", target)

	_, ok = webhook.DeriveTarget("http://localhost:8060/callback", domain.ProfileBitPay)
	assert.False(t, ok)

	_, ok = webhook.DeriveTarget("", domain.ProfileBitPay)
	assert.False(t, ok)
}
