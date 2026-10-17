package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-kit/money"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
)

const profilePaymentID = "01a138fd-0000-7000-8000-000000000004"

func mustProfile(t *testing.T, id string) domain.Profile {
	t.Helper()
	profile, ok := domain.ProfileByID(id)
	require.True(t, ok, "profile %q must exist", id)
	return profile
}

func TestProfileByID(t *testing.T) {
	profile := mustProfile(t, "coingate")
	assert.Equal(t, domain.ProfileCoinGate, profile.ID)
	assert.Equal(t, "Authorization", profile.AuthHeader)
	assert.Equal(t, "Bearer ", profile.AuthPrefix)
	assert.Equal(t, domain.SignatureTokenEcho, profile.Signature.Kind)
	assert.Equal(t, "token", profile.Signature.Field)

	_, ok := domain.ProfileByID("nope")
	assert.False(t, ok)
	_, ok = domain.ProfileByID("")
	assert.False(t, ok)

	profiles := domain.AllProfiles()
	require.Len(t, profiles, 5)
	assert.Equal(t, domain.ProfileMock, profiles[0].ID)
	assert.Equal(t, domain.ProfileBTCPay, profiles[4].ID)
}

func TestProfileRawStatusTables(t *testing.T) {
	tests := []struct {
		profile   string
		canonical domain.Status
		raw       string
	}{
		{"nowpayments", domain.StatusPending, "waiting"},
		{"nowpayments", domain.StatusDetected, "confirming"},
		{"nowpayments", domain.StatusConfirmed, "finished"},
		{"nowpayments", domain.StatusFailed, "failed"},
		{"nowpayments", domain.StatusExpired, "expired"},
		{"nowpayments", domain.StatusUnderpaid, "partially_paid"},
		{"nowpayments", domain.StatusOverpaid, "finished"},
		{"coingate", domain.StatusPending, "new"},
		{"coingate", domain.StatusDetected, "confirming"},
		{"coingate", domain.StatusConfirmed, "paid"},
		{"coingate", domain.StatusFailed, "invalid"},
		{"coingate", domain.StatusExpired, "expired"},
		{"coingate", domain.StatusUnderpaid, "invalid"},
		{"coingate", domain.StatusOverpaid, "paid"},
		{"bitpay", domain.StatusPending, "new"},
		{"bitpay", domain.StatusDetected, "paid"},
		{"bitpay", domain.StatusConfirmed, "complete"},
		{"bitpay", domain.StatusFailed, "invalid"},
		{"bitpay", domain.StatusExpired, "expired"},
		{"bitpay", domain.StatusUnderpaid, "paid"},
		{"bitpay", domain.StatusOverpaid, "paid"},
		{"btcpay", domain.StatusPending, "New"},
		{"btcpay", domain.StatusDetected, "Processing"},
		{"btcpay", domain.StatusConfirmed, "Settled"},
		{"btcpay", domain.StatusFailed, "Invalid"},
		{"btcpay", domain.StatusExpired, "Expired"},
		{"btcpay", domain.StatusUnderpaid, "Processing"},
		{"btcpay", domain.StatusOverpaid, "Settled"},
		{"mock", domain.StatusPending, "pending"},
		{"mock", domain.StatusDetected, "detected"},
		{"mock", domain.StatusConfirmed, "confirmed"},
		{"mock", domain.StatusFailed, "failed"},
		{"mock", domain.StatusExpired, "expired"},
		{"mock", domain.StatusUnderpaid, "underpaid"},
		{"mock", domain.StatusOverpaid, "overpaid"},
	}
	for _, tc := range tests {
		t.Run(tc.profile+"/"+string(tc.canonical), func(t *testing.T) {
			profile := mustProfile(t, tc.profile)
			raw, err := profile.RawStatus(tc.canonical)
			require.NoError(t, err)
			assert.Equal(t, tc.raw, raw)
		})
	}
}

func TestProfileRawStatusesLists(t *testing.T) {
	tests := map[string][]string{
		"nowpayments": {"waiting", "confirming", "confirmed", "sending", "partially_paid", "finished", "failed", "refunded", "expired"},
		"coingate":    {"new", "pending", "confirming", "paid", "invalid", "expired", "canceled", "refunded"},
		"bitpay":      {"new", "paid", "confirmed", "complete", "expired", "invalid"},
		"btcpay":      {"New", "Processing", "Settled", "Expired", "Invalid"},
	}
	for name, list := range tests {
		t.Run(name, func(t *testing.T) {
			profile := mustProfile(t, name)
			assert.ElementsMatch(t, list, profile.RawStatuses)
			assert.True(t, profile.HasRawStatus(list[0]))
			assert.False(t, profile.HasRawStatus("no-such-status"))
			assert.False(t, profile.HasRawStatus(""))
		})
	}
	mock := mustProfile(t, "mock")
	assert.True(t, mock.HasRawStatus("pending"))
	assert.True(t, mock.HasRawStatus("confirmed"))
	assert.False(t, mock.HasRawStatus("finished"))
}

func TestAssetDecimals(t *testing.T) {
	assert.Equal(t, uint32(6), domain.AssetDecimals("USDT"))
	assert.Equal(t, uint32(8), domain.AssetDecimals("BTC"))
	assert.Equal(t, uint32(2), domain.AssetDecimals("XYZ"))
}

func TestParseAssetAmount(t *testing.T) {
	amount, err := domain.ParseAssetAmount("USDT", "12.34")
	require.NoError(t, err)
	assert.Equal(t, money.Money{MinorUnits: 12_340_000, Asset: "USDT", Decimals: 6}, amount)

	amount, err = domain.ParseAssetAmount("USDT", json.Number("12.34"))
	require.NoError(t, err)
	assert.Equal(t, int64(12_340_000), amount.MinorUnits)

	amount, err = domain.ParseAssetAmount("BTC", "0.00000001")
	require.NoError(t, err)
	assert.Equal(t, money.Money{MinorUnits: 1, Asset: "BTC", Decimals: 8}, amount)

	_, err = domain.ParseAssetAmount("USDT", "1.1234567")
	require.Error(t, err)
	assert.True(t, errors.Is(err, money.ErrInvalidAmount))

	_, err = domain.ParseAssetAmount("USDT", 12.34)
	require.Error(t, err)
	assert.True(t, errors.Is(err, money.ErrFloatForbidden))

	_, err = domain.ParseAssetAmount("", "1")
	require.Error(t, err)
}

func TestScenarioValidationExtended(t *testing.T) {
	valid := domain.Scenario{FailNext: 1, LatencyMS: 10, FailStatus: 429, RetryAfterS: 30}
	require.NoError(t, valid.Validate())

	require.NoError(t, domain.Scenario{}.Validate())
	require.NoError(t, domain.Scenario{FailNext: 1, FailStatus: 0}.Validate())

	require.Error(t, domain.Scenario{FailStatus: 418}.Validate())
	require.Error(t, domain.Scenario{FailStatus: 404}.Validate())
	require.Error(t, domain.Scenario{FailStatus: 200}.Validate())
	require.Error(t, domain.Scenario{FailStatus: 429, RetryAfterS: 601}.Validate())
	require.Error(t, domain.Scenario{FailStatus: 500, RetryAfterS: 30}.Validate())
}

func TestProfileParseCreateInvoiceNOWPayments(t *testing.T) {
	profile := mustProfile(t, "nowpayments")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	body := `{"order_id":"` + profilePaymentID + `","price_amount":12.34,"price_currency":"USDT","pay_currency":"usdttrc20","ipn_callback_url":"https://example.test/hooks/nowpayments/ep1"}`
	request, err := profile.ParseCreateInvoice([]byte(body), now)
	require.NoError(t, err)
	assert.Equal(t, profilePaymentID, request.PaymentID)
	assert.Equal(t, profilePaymentID, request.IdempotencyKey)
	assert.Equal(t, money.Money{MinorUnits: 12_340_000, Asset: "USDT", Decimals: 6}, request.Amount)
	assert.Equal(t, "usdttrc20", request.Network)
	assert.Equal(t, now.Add(15*time.Minute), request.ExpiresAt)

	_, err = profile.ParseCreateInvoice([]byte(`{"order_id":"nope","price_amount":1,"price_currency":"USDT","pay_currency":"btc"}`), now)
	require.Error(t, err)

	_, err = profile.ParseCreateInvoice([]byte(`{"order_id":"`+profilePaymentID+`","price_amount":1.1234567,"price_currency":"USDT","pay_currency":"btc"}`), now)
	require.Error(t, err)

	_, err = profile.ParseCreateInvoice([]byte(`{"order_id":"`+profilePaymentID+`","price_amount":1,"price_currency":"USDT","pay_currency":"btc","unknown_field":true}`), now)
	require.Error(t, err)
}

func TestProfileParseCreateInvoiceCoinGate(t *testing.T) {
	profile := mustProfile(t, "coingate")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	body := `{"order_id":"` + profilePaymentID + `","price_amount":"12.34","price_currency":"USDT","pay_currency":"USDT_TRC20"}`
	request, err := profile.ParseCreateInvoice([]byte(body), now)
	require.NoError(t, err)
	assert.Equal(t, int64(12_340_000), request.Amount.MinorUnits)
	assert.Equal(t, "usdt_trc20", request.Network)
}

func TestProfileParseCreateInvoiceBitPay(t *testing.T) {
	profile := mustProfile(t, "bitpay")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	body := `{"token":"tok","price":"12.34","currency":"USDT","orderId":"` + profilePaymentID + `","posData":"usdt_trc20","notificationURL":"https://example.test/hooks/bitpay/ep1"}`
	request, err := profile.ParseCreateInvoice([]byte(body), now)
	require.NoError(t, err)
	assert.Equal(t, money.Money{MinorUnits: 12_340_000, Asset: "USDT", Decimals: 6}, request.Amount)
	assert.Equal(t, "usdt_trc20", request.Network)

	body = `{"token":"tok","price":12.34,"currency":"USDT","orderId":"` + profilePaymentID + `"}`
	request, err = profile.ParseCreateInvoice([]byte(body), now)
	require.NoError(t, err)
	assert.Equal(t, int64(12_340_000), request.Amount.MinorUnits)
	assert.Equal(t, "usdt", request.Network)
}

func TestProfileParseCreateInvoiceBTCPay(t *testing.T) {
	profile := mustProfile(t, "btcpay")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	body := `{"amount":"12.34","currency":"USDT","metadata":{"orderId":"` + profilePaymentID + `","orderNetwork":"trc20"}}`
	request, err := profile.ParseCreateInvoice([]byte(body), now)
	require.NoError(t, err)
	assert.Equal(t, int64(12_340_000), request.Amount.MinorUnits)
	assert.Equal(t, "trc20", request.Network)

	body = `{"amount":"12.34","currency":"USDT","metadata":{"orderId":"` + profilePaymentID + `"}}`
	request, err = profile.ParseCreateInvoice([]byte(body), now)
	require.NoError(t, err)
	assert.Equal(t, "usdt", request.Network)

	_, err = profile.ParseCreateInvoice([]byte(`{"amount":"12.34","currency":"USDT"}`), now)
	require.Error(t, err)
}

func TestProfileResponseShaping(t *testing.T) {
	invoice := domain.Invoice{
		GatewayID:      "gw-1",
		Provider:       domain.ProfileNOWPayments,
		PaymentID:      profilePaymentID,
		Address:        "MOCKADDR",
		Network:        "usdttrc20",
		Amount:         money.MustParse("12.34", "USDT", 6),
		Status:         domain.StatusPending,
		RawStatus:      "waiting",
		ReceivedAmount: money.Money{Asset: "USDT", Decimals: 6},
		CreatedAt:      time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
		ExpiresAt:      time.Date(2026, 10, 17, 12, 15, 0, 0, time.UTC),
	}
	profile := mustProfile(t, "nowpayments")

	payload, err := profile.CreateInvoiceResponse(invoice)
	require.NoError(t, err)
	decoded := decodeObject(t, payload)
	assert.Equal(t, "gw-1", decoded["id"])
	assert.Equal(t, profilePaymentID, decoded["order_id"])
	assert.Equal(t, json.Number("12.340000"), decoded["price_amount"])
	assert.Equal(t, "USDT", decoded["price_currency"])
	assert.Equal(t, "usdttrc20", decoded["pay_currency"])
	assert.NotEmpty(t, decoded["invoice_url"])

	payload, err = profile.PaymentResponse(invoice)
	require.NoError(t, err)
	decoded = decodeObject(t, payload)
	assert.Equal(t, "gw-1", decoded["payment_id"])
	assert.Equal(t, "waiting", decoded["payment_status"])
	assert.Equal(t, json.Number("12.340000"), decoded["price_amount"])
	assert.Equal(t, json.Number("0.000000"), decoded["actually_paid"])

	coingate := mustProfile(t, "coingate")
	coingateInvoice := invoice
	coingateInvoice.Provider = domain.ProfileCoinGate
	coingateInvoice.GatewayID = "1042"
	coingateInvoice.RawStatus = "new"
	payload, err = coingate.CreateInvoiceResponse(coingateInvoice)
	require.NoError(t, err)
	decoded = decodeObject(t, payload)
	assert.Equal(t, json.Number("1042"), decoded["id"])
	assert.Equal(t, "new", decoded["status"])
	assert.Equal(t, "12.340000", decoded["price_amount"])
	assert.NotEmpty(t, decoded["payment_address"])

	bitpay := mustProfile(t, "bitpay")
	bitpayInvoice := invoice
	bitpayInvoice.Provider = domain.ProfileBitPay
	bitpayInvoice.GatewayID = "abcdefghijklmnopqrstuv"
	bitpayInvoice.RawStatus = "new"
	payload, err = bitpay.CreateInvoiceResponse(bitpayInvoice)
	require.NoError(t, err)
	decoded = decodeObject(t, payload)
	assert.Equal(t, "merchant/invoice", decoded["facade"])
	data, ok := decoded["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "abcdefghijklmnopqrstuv", data["id"])
	assert.Equal(t, json.Number("12.340000"), data["price"])
	assert.Equal(t, json.Number(strconv.FormatInt(bitpayInvoice.CreatedAt.UnixMilli(), 10)), data["invoiceTime"])

	btcpay := mustProfile(t, "btcpay")
	btcpayInvoice := invoice
	btcpayInvoice.Provider = domain.ProfileBTCPay
	btcpayInvoice.GatewayID = "0123456789abcdef0123456789abcdef"
	btcpayInvoice.RawStatus = "New"
	payload, err = btcpay.CreateInvoiceResponse(btcpayInvoice)
	require.NoError(t, err)
	decoded = decodeObject(t, payload)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", decoded["id"])
	assert.Equal(t, "New", decoded["status"])
	assert.Equal(t, "12.340000", decoded["amount"])
	assert.NotEmpty(t, decoded["checkoutLink"])
}

func TestValidateAuth(t *testing.T) {
	header := func(values map[string]string) http.Header {
		h := http.Header{}
		for key, value := range values {
			h.Set(key, value)
		}
		return h
	}
	now := mustProfile(t, "nowpayments")
	require.Error(t, now.ValidateAuth(header(nil)))
	require.Error(t, now.ValidateAuth(header(map[string]string{"x-api-key": "  "})))
	require.NoError(t, now.ValidateAuth(header(map[string]string{"x-api-key": "k"})))

	coingate := mustProfile(t, "coingate")
	require.Error(t, coingate.ValidateAuth(header(map[string]string{"Authorization": "Basic abc"})))
	require.Error(t, coingate.ValidateAuth(header(map[string]string{"Authorization": "Bearer "})))
	require.NoError(t, coingate.ValidateAuth(header(map[string]string{"Authorization": "Bearer token"})))

	btcpay := mustProfile(t, "btcpay")
	require.Error(t, btcpay.ValidateAuth(header(map[string]string{"Authorization": "Bearer x"})))
	require.NoError(t, btcpay.ValidateAuth(header(map[string]string{"Authorization": "token key"})))

	bitpay := mustProfile(t, "bitpay")
	require.Error(t, bitpay.ValidateAuth(header(map[string]string{"X-Identity": "id"})))
	require.Error(t, bitpay.ValidateAuth(header(map[string]string{"X-Signature": "sig"})))
	require.NoError(t, bitpay.ValidateAuth(header(map[string]string{"X-Identity": "02abcd", "X-Signature": "3045"})))
}

func TestRawStatusUnsupported(t *testing.T) {
	empty := domain.Profile{ID: "custom"}
	_, err := empty.RawStatus(domain.StatusDetected)
	require.Error(t, err)
	assert.False(t, empty.HasRawStatus("anything"))
}

func TestPaymentResponsePerProfile(t *testing.T) {
	base := domain.Invoice{
		GatewayID:      "gw-1",
		PaymentID:      profilePaymentID,
		Address:        "MOCKADDR",
		Network:        "usdt_trc20",
		Amount:         money.MustParse("12.34", "USDT", 6),
		Status:         domain.StatusConfirmed,
		RawStatus:      "paid",
		ReceivedAmount: money.MustParse("12.00", "USDT", 6),
		CreatedAt:      time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
		ExpiresAt:      time.Date(2026, 10, 17, 12, 15, 0, 0, time.UTC),
	}

	coingate := mustProfile(t, "coingate")
	invoice := base
	invoice.Provider = domain.ProfileCoinGate
	invoice.GatewayID = "1042"
	payload, err := coingate.PaymentResponse(invoice)
	require.NoError(t, err)
	decoded := decodeObject(t, payload)
	assert.Equal(t, "paid", decoded["status"])
	assert.Equal(t, "12.000000", decoded["pay_amount"])

	bitpay := mustProfile(t, "bitpay")
	invoice = base
	invoice.Provider = domain.ProfileBitPay
	invoice.RawStatus = "complete"
	payload, err = bitpay.PaymentResponse(invoice)
	require.NoError(t, err)
	decoded = decodeObject(t, payload)
	data, ok := decoded["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "complete", data["status"])
	assert.Equal(t, json.Number("12.340000"), data["price"])

	btcpay := mustProfile(t, "btcpay")
	invoice = base
	invoice.Provider = domain.ProfileBTCPay
	invoice.RawStatus = "Settled"
	payload, err = btcpay.PaymentResponse(invoice)
	require.NoError(t, err)
	decoded = decodeObject(t, payload)
	assert.Equal(t, "Settled", decoded["status"])
	assert.Equal(t, "12.340000", decoded["amount"])

	// Zero received amounts fall back to the invoice amount on the wire.
	invoice = base
	invoice.Provider = domain.ProfileBitPay
	invoice.ReceivedAmount = money.Money{}
	payload, err = bitpay.PaymentResponse(invoice)
	require.NoError(t, err)
	decoded = decodeObject(t, payload)
	data, ok = decoded["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, json.Number("12.340000"), data["amountPaid"])

	// The mock profile has no profile API: the legacy routes serve it.
	mock := mustProfile(t, "mock")
	_, err = mock.PaymentResponse(base)
	require.Error(t, err)
	_, err = mock.CreateInvoiceResponse(base)
	require.Error(t, err)
}

func decodeObject(t *testing.T, payload any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded map[string]any
	require.NoError(t, decoder.Decode(&decoded))
	return decoded
}
