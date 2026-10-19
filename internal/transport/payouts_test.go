package transport_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pullPaymentPath builds the pull payment route of the btcpay profile.
func pullPaymentPath() string { return "/api/v1/stores/mock-store/pull-payments" }

// payoutCreatePath builds the payout creation route of a pull payment.
func payoutCreatePath(pullPaymentID string) string {
	return "/api/v1/pull-payments/" + pullPaymentID + "/payouts"
}

// payoutReadPath builds the payout read route.
func payoutReadPath(payoutID string) string { return "/api/v1/stores/mock-store/payouts/" + payoutID }

// payoutSimulatePath builds the stand payout state control route.
func payoutSimulatePath(payoutID string) string { return "/api/v1/payouts/" + payoutID + "/simulate" }

// invoiceListPath builds the store invoice list route.
func invoiceListPath() string { return "/api/v1/stores/mock-store/invoices" }

// pullPaymentBody builds a pull payment creation body.
func pullPaymentBody(name string) string {
	return `{"name":"` + name + `","amount":"0.00100000","currency":"BTC","payoutMethods":["BTC"]}`
}

// payoutBody builds a payout creation body.
func payoutBody() string {
	return `{"destination":"bcrt1qexample","amount":"0.00100000","payoutMethodId":"BTC-CHAIN"}`
}

// createPullPayment creates a pull payment and returns its id.
func createPullPayment(t *testing.T, router *gin.Engine, name string) string {
	t.Helper()
	recorder := doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody(name), authHeaders("btcpay"))
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	return decodeProfileResponse(t, recorder)["id"].(string)
}

// createPayout creates a payout and returns its id.
func createPayout(t *testing.T, router *gin.Engine, pullPaymentID string) string {
	t.Helper()
	recorder := doJSON(router, http.MethodPost, payoutCreatePath(pullPaymentID), payoutBody(), authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	return decodeProfileResponse(t, recorder)["id"].(string)
}

// createProfileOrderWithID creates a btcpay invoice with an explicit order id
// and returns the gateway id.
func createProfileOrderWithID(t *testing.T, router *gin.Engine, orderID string) string {
	t.Helper()
	recorder := doJSON(router, http.MethodPost, createPath("btcpay"), orderBody("btcpay", orderID), authHeaders("btcpay"))
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	id, _ := decodeProfileResponse(t, recorder)["id"].(string)
	require.NotEmpty(t, id)
	return id
}

func TestPayoutAuthAndPullPaymentCreate(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	missing := doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody("pp-1"), nil)
	assert.Equal(t, http.StatusUnauthorized, missing.Code)

	recorder := doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody("pp-1"), authHeaders("btcpay"))
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	decoded := decodeProfileResponse(t, recorder)
	assert.NotEmpty(t, decoded["id"])
	assert.Equal(t, "pp-1", decoded["name"])
	assert.Equal(t, "0.00100000", decoded["amount"])
	assert.Equal(t, "BTC", decoded["currency"])
}

func TestPullPaymentReplayByName(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	first := createPullPayment(t, router, "pp-replay")
	second := doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody("pp-replay"), authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.Equal(t, first, decodeProfileResponse(t, second)["id"])
}

func TestRefundNameReplayThenDistinctPayouts(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	name := "refund-01a138fd-0000-7000-8000-000000000073"
	first := createPullPayment(t, router, name)
	replayed := doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody(name), authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, replayed.Code, replayed.Body.String())
	require.Equal(t, first, decodeProfileResponse(t, replayed)["id"], "the refund pull payment replays by name")

	one := createPayout(t, router, first)
	two := createPayout(t, router, first)
	assert.NotEqual(t, one, two, "a payout create is not deduplicated by the pull payment name")
}

func TestPayoutCreateAndRead(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	pullPaymentID := createPullPayment(t, router, "pp-create")
	payoutID := createPayout(t, router, pullPaymentID)

	read := doJSON(router, http.MethodGet, payoutReadPath(payoutID), "", authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	decoded := decodeProfileResponse(t, read)
	assert.Equal(t, payoutID, decoded["id"])
	assert.Equal(t, "AwaitingApproval", decoded["state"])
	assert.Equal(t, pullPaymentID, decoded["pullPaymentId"])
}

func TestPayoutCreateUnknownPullPayment(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	recorder := doJSON(router, http.MethodPost, payoutCreatePath("01a138fd-ffff-7000-8000-00000000ffff"), payoutBody(), authHeaders("btcpay"))
	require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "pull_payment_not_found")
}

func TestPayoutReadUnknown(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	recorder := doJSON(router, http.MethodGet, payoutReadPath("01a138fd-ffff-7000-8000-00000000ffff"), "", authHeaders("btcpay"))
	require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "payout_not_found")
}

func TestPayoutSimulateStates(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	payoutID := createPayout(t, router, createPullPayment(t, router, "pp-simulate"))

	recorder := doJSON(router, http.MethodPost, payoutSimulatePath(payoutID), `{"state":"Completed"}`, authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, "Completed", decodeProfileResponse(t, recorder)["state"])

	read := doJSON(router, http.MethodGet, payoutReadPath(payoutID), "", authHeaders("btcpay"))
	assert.Equal(t, "Completed", decodeProfileResponse(t, read)["state"])

	invalid := doJSON(router, http.MethodPost, payoutSimulatePath(payoutID), `{"state":"paid"}`, authHeaders("btcpay"))
	require.Equal(t, http.StatusBadRequest, invalid.Code, invalid.Body.String())
	assert.Contains(t, invalid.Body.String(), "invalid_request")

	unchanged := doJSON(router, http.MethodGet, payoutReadPath(payoutID), "", authHeaders("btcpay"))
	assert.Equal(t, "Completed", decodeProfileResponse(t, unchanged)["state"], "a rejected state word must not change the payout")
}

func TestInvoiceListWindowAndPaging(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	first := createProfileOrderWithID(t, router, "01a138fd-0000-7000-8000-000000000061")
	second := createProfileOrderWithID(t, router, "01a138fd-0000-7000-8000-000000000062")

	// A mock-profile invoice must never appear in the btcpay store list.
	legacy := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody(), nil)
	require.Equal(t, http.StatusCreated, legacy.Code, legacy.Body.String())

	now := time.Now().UTC().Unix()
	window := invoiceListPath() + "?startDate=" + itoa(now-3600) + "&endDate=" + itoa(now+60)
	all := doJSON(router, http.MethodGet, window, "", authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, all.Code, all.Body.String())
	rows := decodeInvoiceRows(t, all)
	require.Len(t, rows, 2)
	ids := []string{rows[0]["id"].(string), rows[1]["id"].(string)}
	assert.ElementsMatch(t, []string{first, second}, ids)
	assert.Equal(t, "New", rows[0]["status"])

	pageOne := doJSON(router, http.MethodGet, window+"&take=1", "", authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, pageOne.Code)
	assert.Len(t, decodeInvoiceRows(t, pageOne), 1)
	pageTwo := doJSON(router, http.MethodGet, window+"&take=1&skip=1", "", authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, pageTwo.Code)
	assert.Len(t, decodeInvoiceRows(t, pageTwo), 1)

	past := invoiceListPath() + "?startDate=" + itoa(now-7200) + "&endDate=" + itoa(now-3600)
	empty := doJSON(router, http.MethodGet, past, "", authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, empty.Code)
	assert.Equal(t, "[]", empty.Body.String())
}

func TestInvoiceListValidation(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	cases := []string{
		invoiceListPath(),
		invoiceListPath() + "?startDate=100",
		invoiceListPath() + "?startDate=200&endDate=100",
		invoiceListPath() + "?startDate=abc&endDate=100",
		invoiceListPath() + "?startDate=100&endDate=200&take=abc",
		invoiceListPath() + "?startDate=100&endDate=200&skip=-1",
	}
	for _, path := range cases {
		recorder := doJSON(router, http.MethodGet, path, "", authHeaders("btcpay"))
		require.Equal(t, http.StatusBadRequest, recorder.Code, path)
		assert.Contains(t, recorder.Body.String(), "invalid_request")
	}
}

func TestInvoiceListOrderLookup(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	orderID := "01a138fd-0000-7000-8000-000000000071"
	gatewayID := createProfileOrderWithID(t, router, orderID)

	recorder := doJSON(router, http.MethodGet, invoiceListPath()+"?orderId="+orderID, "", authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	rows := decodeInvoiceRows(t, recorder)
	require.Len(t, rows, 1)
	assert.Equal(t, gatewayID, rows[0]["id"])

	unknown := doJSON(router, http.MethodGet, invoiceListPath()+"?orderId=01a138fd-ffff-7000-8000-00000000ffff", "", authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, unknown.Code)
	assert.Equal(t, "[]", unknown.Body.String(), "an unknown order keeps the connector not-found classification")
}

func TestInvoiceListSkipPastEnd(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	createProfileOrderWithID(t, router, "01a138fd-0000-7000-8000-000000000074")
	now := time.Now().UTC().Unix()
	window := invoiceListPath() + "?startDate=" + itoa(now-3600) + "&endDate=" + itoa(now+60) + "&skip=100"
	recorder := doJSON(router, http.MethodGet, window, "", authHeaders("btcpay"))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, "[]", recorder.Body.String())
}

func TestSilentSimulateSkipsWebhooks(t *testing.T) {
	router, sender := newProfileRouter(t, false)
	gatewayID := createProfileOrder(t, router, "btcpay")

	body := `{"gateway_id":"` + gatewayID + `","provider":"btcpay","status":"confirmed","silent":true}`
	recorder := doJSON(router, http.MethodPost, "/api/v1/simulate", body, nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	decoded := decodeProfileResponse(t, recorder)
	assert.Empty(t, decoded["event_id"])
	assert.Empty(t, decoded["deliveries"])
	assert.Empty(t, sender.events, "a silent simulate must not send webhooks")

	read := doJSON(router, http.MethodGet, "/api/v1/stores/mock-store/invoices/"+gatewayID, "", authHeaders("btcpay"))
	assert.Equal(t, "Settled", decodeProfileResponse(t, read)["status"])

	// A silent simulate must not require a webhook target either.
	missingRouter, _ := newProfileRouter(t, true)
	silentMissing := doJSON(missingRouter, http.MethodPost, "/api/v1/simulate",
		`{"gateway_id":"`+createProfileOrder(t, missingRouter, "btcpay")+`","provider":"btcpay","status":"confirmed","silent":true}`, nil)
	assert.Equal(t, http.StatusOK, silentMissing.Code, silentMissing.Body.String())
}

func TestSilentSimulateProviderMismatch(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	gatewayID := createProfileOrderWithID(t, router, "01a138fd-0000-7000-8000-000000000072")
	body := `{"gateway_id":"` + gatewayID + `","provider":"coingate","status":"confirmed","silent":true}`
	recorder := doJSON(router, http.MethodPost, "/api/v1/simulate", body, nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "provider_mismatch")
}

func TestPayoutScenarioInjection(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	set := doJSON(router, http.MethodPost, "/api/v1/scenarios", `{"fail_next":1,"fail_status":400}`, nil)
	require.Equal(t, http.StatusOK, set.Code)

	recorder := doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody("pp-injected"), authHeaders("btcpay"))
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "provider_unavailable")

	recovered := doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody("pp-injected"), authHeaders("btcpay"))
	assert.Equal(t, http.StatusCreated, recovered.Code, recovered.Body.String())

	// The rate-limit injection carries the Retry-After header.
	set = doJSON(router, http.MethodPost, "/api/v1/scenarios", `{"fail_next":1,"fail_status":429,"retry_after_s":7}`, nil)
	require.Equal(t, http.StatusOK, set.Code)
	limited := doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody("pp-429"), authHeaders("btcpay"))
	require.Equal(t, http.StatusTooManyRequests, limited.Code, limited.Body.String())
	assert.Equal(t, "7", limited.Header().Get("Retry-After"))

	// The latency injection delays the payout call.
	set = doJSON(router, http.MethodPost, "/api/v1/scenarios", `{"latency_ms":20}`, nil)
	require.Equal(t, http.StatusOK, set.Code)
	start := time.Now()
	delayed := doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody("pp-latency"), authHeaders("btcpay"))
	require.Equal(t, http.StatusCreated, delayed.Code, delayed.Body.String())
	assert.GreaterOrEqual(t, time.Since(start).Milliseconds(), int64(15))

	reset := doJSON(router, http.MethodPost, "/api/v1/scenarios/reset", "", nil)
	require.Equal(t, http.StatusOK, reset.Code)
}

func TestPayoutStoreCap(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	var last *httptest.ResponseRecorder
	for i := 0; i <= 1000; i++ {
		last = doJSON(router, http.MethodPost, pullPaymentPath(), pullPaymentBody("pp-cap-"+itoa(int64(i))), authHeaders("btcpay"))
	}
	require.Equal(t, http.StatusServiceUnavailable, last.Code, last.Body.String())
	assert.Contains(t, last.Body.String(), "store_full")

	capped, _ := newProfileRouter(t, false)
	pull := createPullPayment(t, capped, "pp-payout-cap")
	for i := 0; i <= 1000; i++ {
		last = doJSON(capped, http.MethodPost, payoutCreatePath(pull), payoutBody(), authHeaders("btcpay"))
	}
	require.Equal(t, http.StatusServiceUnavailable, last.Code, last.Body.String())
	assert.Contains(t, last.Body.String(), "store_full")
}

func TestCrossProfilePayoutRead(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	legacy := doJSON(router, http.MethodPost, "/api/v1/invoices", createBody(), nil)
	require.Equal(t, http.StatusCreated, legacy.Code)
	gatewayID := decodeInvoice(t, legacy)

	recorder := doJSON(router, http.MethodGet, payoutReadPath(gatewayID), "", authHeaders("btcpay"))
	assert.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
}

func TestPayoutRoutesRejectMalformedBodies(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	pull := createPullPayment(t, router, "pp-malformed")
	payoutID := createPayout(t, router, pull)

	cases := []struct {
		path string
		code string
	}{
		{pullPaymentPath(), "invalid_request"},
		{payoutCreatePath(pull), "invalid_request"},
		{payoutSimulatePath(payoutID), "invalid_json"},
	}
	for _, tc := range cases {
		recorder := doJSON(router, http.MethodPost, tc.path, "{not json", authHeaders("btcpay"))
		require.Equal(t, http.StatusBadRequest, recorder.Code, tc.path)
		assert.Contains(t, recorder.Body.String(), tc.code)
	}
}

func TestInvoiceListInjectedFailure(t *testing.T) {
	router, _ := newProfileRouter(t, false)
	set := doJSON(router, http.MethodPost, "/api/v1/scenarios", `{"fail_next":1,"fail_status":503}`, nil)
	require.Equal(t, http.StatusOK, set.Code)

	now := time.Now().UTC().Unix()
	window := invoiceListPath() + "?startDate=" + itoa(now-3600) + "&endDate=" + itoa(now+60)
	recorder := doJSON(router, http.MethodGet, window, "", authHeaders("btcpay"))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "provider_unavailable")
}

// decodeInvoiceRows decodes a store invoice list body.
func decodeInvoiceRows(t *testing.T, recorder *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(recorder.Body)
	decoder.UseNumber()
	var rows []map[string]any
	require.NoError(t, decoder.Decode(&rows))
	return rows
}

// itoa renders an integer for query strings.
func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
