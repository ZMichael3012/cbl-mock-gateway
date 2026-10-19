//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/transport"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
	"github.com/cbl/cbl-mock-gateway/internal/webhook"
)

// TestE2EMockGatewayPayoutExport exercises the BTCPay payout, refund and export
// emulation end to end (CBL-17.27): the invoice list walk used by the connector
// export, the pull payment plus payout flow of the payout pipeline, the stand
// payout state control and the silent simulate that models a lost webhook.
func TestE2EMockGatewayPayoutExport(t *testing.T) {
	sink := &profileSink{verifiers: strategies(), records: map[string][]profileRecord{}}
	sinkSrv := httptest.NewServer(sink)
	defer sinkSrv.Close()

	profile, ok := domain.ProfileByID("btcpay")
	require.True(t, ok)
	sender, err := webhook.NewProfileSender(profile, sinkSrv.URL+"/btcpay", strategies()["btcpay"].secret, idHeader, 3*time.Second)
	require.NoError(t, err)
	senders := map[domain.ProfileID]usecase.WebhookSender{domain.ProfileBTCPay: sender}
	switcher, err := webhook.NewSwitch(senders)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	service := usecase.NewService(switcher, nil, usecase.WithTargets(switcher))
	transport.NewServer(service, nil).Register(router)
	gatewaySrv := httptest.NewServer(router)
	defer gatewaySrv.Close()

	auth := map[string]string{"Authorization": "token btc-key"}
	orderID := "01a138fd-0000-7000-8000-000000000051"

	created := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/stores/store-1/invoices",
		`{"amount":"0.00100000","currency":"BTC","metadata":{"orderId":"`+orderID+`","orderNetwork":"btc"}}`, auth)
	require.Equal(t, http.StatusCreated, created.code, string(created.payload))
	gatewayID := gatewayIDFromProfile(t, created.payload)

	now := time.Now().UTC().Unix()
	window := fmt.Sprintf("%s/api/v1/stores/store-1/invoices?startDate=%d&endDate=%d", gatewaySrv.URL, now-3600, now+60)
	list := postProfileJSON(t, http.MethodGet, window, "", auth)
	require.Equal(t, http.StatusOK, list.code, string(list.payload))
	rows := decodeListRows(t, list.payload)
	require.Len(t, rows, 1)
	assert.Equal(t, gatewayID, rows[0]["id"])
	assert.Equal(t, "New", rows[0]["status"])
	metadata, ok := rows[0]["metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, orderID, metadata["orderId"])

	pull := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/stores/store-1/pull-payments",
		`{"name":"`+orderID+`","amount":"0.00100000","currency":"BTC","payoutMethods":["BTC"]}`, auth)
	require.Equal(t, http.StatusCreated, pull.code, string(pull.payload))
	pullPaymentID := gatewayIDFromProfile(t, pull.payload)

	payout := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/pull-payments/"+pullPaymentID+"/payouts",
		`{"destination":"bcrt1qexample","amount":"0.00100000","payoutMethodId":"BTC-CHAIN"}`, auth)
	require.Equal(t, http.StatusOK, payout.code, string(payout.payload))
	payoutID := gatewayIDFromProfile(t, payout.payload)

	read := postProfileJSON(t, http.MethodGet, gatewaySrv.URL+"/api/v1/stores/store-1/payouts/"+payoutID, "", auth)
	require.Equal(t, http.StatusOK, read.code, string(read.payload))
	assert.Contains(t, string(read.payload), "AwaitingApproval")

	advanced := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/payouts/"+payoutID+"/simulate", `{"state":"Completed"}`, auth)
	require.Equal(t, http.StatusOK, advanced.code, string(advanced.payload))
	assert.Contains(t, string(advanced.payload), "Completed")

	settled := postProfileJSON(t, http.MethodGet, gatewaySrv.URL+"/api/v1/stores/store-1/payouts/"+payoutID, "", auth)
	require.Equal(t, http.StatusOK, settled.code, string(settled.payload))
	assert.Contains(t, string(settled.payload), "Completed", "the polled payout read must reflect the simulated state")

	// A silent simulate changes the provider state only: no webhook is sent and
	// the export shows the divergence the reconciler must detect.
	silent := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","provider":"btcpay","status":"confirmed","received_amount":{"minor_units":90000,"asset":"BTC","decimals":8},"silent":true}`, nil)
	require.Equal(t, http.StatusOK, silent.code, string(silent.payload))
	assert.Empty(t, sink.recordsOf("btcpay"), "a silent simulate must not deliver webhooks")

	listAgain := postProfileJSON(t, http.MethodGet, window, "", auth)
	require.Equal(t, http.StatusOK, listAgain.code)
	rowsAgain := decodeListRows(t, listAgain.payload)
	require.Len(t, rowsAgain, 1)
	assert.Equal(t, "Settled", rowsAgain[0]["status"])
	assert.Equal(t, "0.00090000", rowsAgain[0]["amountPaid"])

	normal := postProfileJSON(t, http.MethodPost, gatewaySrv.URL+"/api/v1/simulate",
		`{"gateway_id":"`+gatewayID+`","provider":"btcpay","status":"confirmed"}`, nil)
	require.Equal(t, http.StatusOK, normal.code, string(normal.payload))
	records := sink.recordsOf("btcpay")
	require.Len(t, records, 1)
	assert.True(t, records[0].verified, "the normal simulate must deliver a verified webhook")
}

// decodeListRows decodes a store invoice list body.
func decodeListRows(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var rows []map[string]any
	require.NoError(t, decoder.Decode(&rows))
	return rows
}
