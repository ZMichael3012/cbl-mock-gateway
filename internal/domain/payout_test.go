package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZMichael3012/cbl-kit/money"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
)

const payoutPullPaymentID = "01a138fd-0000-7000-8000-000000000041"

// mustBTC asserts and returns a money value for the payout tests.
func mustBTC(t *testing.T, literal string) money.Money {
	t.Helper()
	parsed, err := money.Parse(literal, "BTC", 8)
	require.NoError(t, err)
	return parsed
}

func TestParsePullPaymentCreateValid(t *testing.T) {
	profile := mustProfile(t, "btcpay")
	body := []byte(`{"name":"` + payoutPullPaymentID + `","amount":"0.00100000","currency":"BTC","payoutMethods":["BTC"]}`)
	request, err := profile.ParsePullPaymentCreate(body)
	require.NoError(t, err)
	assert.Equal(t, payoutPullPaymentID, request.Name)
	assert.Equal(t, mustBTC(t, "0.00100000"), request.Amount)
	assert.Equal(t, []string{"BTC"}, request.PayoutMethods)
}

func TestParsePullPaymentCreateInvalid(t *testing.T) {
	profile := mustProfile(t, "btcpay")
	cases := map[string]string{
		"empty name":           `{"name":"","amount":"0.00100000","currency":"BTC","payoutMethods":["BTC"]}`,
		"negative amount":      `{"name":"n-1","amount":"-0.00100000","currency":"BTC","payoutMethods":["BTC"]}`,
		"zero amount":          `{"name":"n-1","amount":"0","currency":"BTC","payoutMethods":["BTC"]}`,
		"scale beyond asset":   `{"name":"n-1","amount":"0.00100000","currency":"XX","payoutMethods":["BTC"]}`,
		"short currency":       `{"name":"n-1","amount":"0.001","currency":"X","payoutMethods":["BTC"]}`,
		"empty methods":        `{"name":"n-1","amount":"0.00100000","currency":"BTC","payoutMethods":[]}`,
		"too many methods":     `{"name":"n-1","amount":"0.00100000","currency":"BTC","payoutMethods":["BTC","BTC","BTC","BTC","BTC","BTC","BTC","BTC","BTC","BTC","BTC"]}`,
		"invalid method chars": `{"name":"n-1","amount":"0.00100000","currency":"BTC","payoutMethods":["BT C"]}`,
		"unknown field":        `{"name":"n-1","amount":"0.00100000","currency":"BTC","payoutMethods":["BTC"],"extra":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := profile.ParsePullPaymentCreate([]byte(body))
			require.ErrorIs(t, err, domain.ErrInvalidRequest)
		})
	}
}

func TestPullPaymentRequestValidateBounds(t *testing.T) {
	methods := make([]string, 11)
	for i := range methods {
		methods[i] = "BTC"
	}
	err := domain.PullPaymentRequest{Name: "n-1", Amount: mustBTC(t, "0.001"), PayoutMethods: methods}.Validate()
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
}

func TestParsePayoutCreateValid(t *testing.T) {
	profile := mustProfile(t, "btcpay")
	body := []byte(`{"destination":"bcrt1qexample","amount":"0.00100000","payoutMethodId":"BTC-CHAIN"}`)
	request, err := profile.ParsePayoutCreate(body)
	require.NoError(t, err)
	assert.Equal(t, "bcrt1qexample", request.Destination)
	assert.Equal(t, mustBTC(t, "0.00100000"), request.Amount)
	assert.Equal(t, "BTC-CHAIN", request.MethodID)
}

func TestParsePayoutCreateInvalid(t *testing.T) {
	profile := mustProfile(t, "btcpay")
	cases := map[string]string{
		"empty destination": `{"destination":"","amount":"0.00100000","payoutMethodId":"BTC-CHAIN"}`,
		"zero amount":       `{"destination":"bcrt1qexample","amount":"0","payoutMethodId":"BTC-CHAIN"}`,
		"empty method":      `{"destination":"bcrt1qexample","amount":"0.00100000","payoutMethodId":""}`,
		"bad method chars":  `{"destination":"bcrt1qexample","amount":"0.00100000","payoutMethodId":"BT C-CHAIN"}`,
		"unknown field":     `{"destination":"bcrt1qexample","amount":"0.00100000","payoutMethodId":"BTC-CHAIN","extra":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := profile.ParsePayoutCreate([]byte(body))
			require.ErrorIs(t, err, domain.ErrInvalidRequest)
		})
	}
}

func TestPayoutWireResponses(t *testing.T) {
	profile := mustProfile(t, "btcpay")
	pull, err := profile.PullPaymentResponse(domain.PullPayment{
		ID:            "01a138fdaa",
		Name:          payoutPullPaymentID,
		Amount:        mustBTC(t, "0.00100000"),
		PayoutMethods: []string{"BTC"},
	})
	require.NoError(t, err)
	pullBody, err := json.Marshal(pull)
	require.NoError(t, err)
	var pullDecoded map[string]any
	require.NoError(t, json.Unmarshal(pullBody, &pullDecoded))
	assert.Equal(t, "01a138fdaa", pullDecoded["id"])
	assert.Equal(t, payoutPullPaymentID, pullDecoded["name"])
	assert.Equal(t, "0.00100000", pullDecoded["amount"])
	assert.Equal(t, "BTC", pullDecoded["currency"])
	assert.Equal(t, []any{"BTC"}, pullDecoded["payoutMethods"])
	assert.NotEmpty(t, pullDecoded["viewLink"])

	payout, err := profile.PayoutResponse(domain.Payout{
		ID:            "01a138fdbb",
		PullPaymentID: "01a138fdaa",
		Destination:   "bcrt1qexample",
		Amount:        mustBTC(t, "0.00100000"),
		MethodID:      "BTC-CHAIN",
		State:         domain.PayoutStateAwaitingApproval,
	})
	require.NoError(t, err)
	payoutBody, err := json.Marshal(payout)
	require.NoError(t, err)
	var payoutDecoded map[string]any
	require.NoError(t, json.Unmarshal(payoutBody, &payoutDecoded))
	assert.Equal(t, "01a138fdbb", payoutDecoded["id"])
	assert.Equal(t, domain.PayoutStateAwaitingApproval, payoutDecoded["state"])
	assert.Equal(t, "01a138fdaa", payoutDecoded["pullPaymentId"])
}

func TestValidPayoutState(t *testing.T) {
	for _, state := range []string{"AwaitingApproval", "AwaitingPayment", "InProgress", "Completed", "Cancelled"} {
		assert.True(t, domain.ValidPayoutState(state), state)
	}
	for _, state := range []string{"", "completed", "Paid", "Settled"} {
		assert.False(t, domain.ValidPayoutState(state), state)
	}
}

func TestPayoutAPIsAreBTCPayOnly(t *testing.T) {
	profile := mustProfile(t, "coingate")
	_, err := profile.ParsePullPaymentCreate([]byte(`{}`))
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
	_, err = profile.ParsePayoutCreate([]byte(`{}`))
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
	_, err = profile.PullPaymentResponse(domain.PullPayment{})
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
	_, err = profile.PayoutResponse(domain.Payout{})
	require.ErrorIs(t, err, domain.ErrInvalidRequest)
}

func TestAllowsPayout(t *testing.T) {
	pull := domain.PullPayment{
		PayoutMethods: []string{"BTC"},
		Amount:        mustBTC(t, "0.00100000"),
	}
	ok := domain.PayoutRequest{Destination: "bcrt1qexample", Amount: mustBTC(t, "0.00100000"), MethodID: "BTC-CHAIN"}
	require.NoError(t, pull.AllowsPayout(ok))

	foreignMethod := ok
	foreignMethod.MethodID = "ETH-CHAIN"
	require.ErrorIs(t, pull.AllowsPayout(foreignMethod), domain.ErrInvalidRequest)

	overAmount := ok
	overAmount.Amount = mustBTC(t, "0.00200000")
	require.ErrorIs(t, pull.AllowsPayout(overAmount), domain.ErrInvalidRequest)

	foreignAsset := ok
	foreignAsset.Amount = money.Money{MinorUnits: 100_000, Asset: "USDT", Decimals: 6}
	require.ErrorIs(t, pull.AllowsPayout(foreignAsset), domain.ErrInvalidRequest)
}

func TestFilterInvoicesWindowAndOrder(t *testing.T) {
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	invoices := []domain.Invoice{
		{GatewayID: "bb", CreatedAt: base.Add(-2 * time.Hour)},
		{GatewayID: "cc", CreatedAt: base.Add(-1 * time.Hour)},
		{GatewayID: "aa", CreatedAt: base.Add(-1 * time.Hour)},
		{GatewayID: "dd", CreatedAt: base.Add(-30 * time.Minute)},
	}
	filtered := domain.FilterInvoices(invoices, base.Add(-90*time.Minute), base.Add(-15*time.Minute), 0, 0)
	require.Len(t, filtered, 3)
	assert.Equal(t, []string{"aa", "cc", "dd"}, []string{filtered[0].GatewayID, filtered[1].GatewayID, filtered[2].GatewayID})

	// The window is inclusive on both ends.
	edge := domain.FilterInvoices(invoices, base.Add(-2*time.Hour), base.Add(-30*time.Minute), 0, 0)
	assert.Len(t, edge, 4)

	// The window compares at the wire granularity (unix seconds): an invoice at
	// endDate+900ms still carries createdTime == endDate and must be included.
	subSecond := []domain.Invoice{{GatewayID: "ee", CreatedAt: base.Add(900 * time.Millisecond)}}
	included := domain.FilterInvoices(subSecond, base.Add(-time.Hour), base, 0, 0)
	assert.Len(t, included, 1, "the sub-second part must not fall outside the inclusive window")
}

func TestFilterInvoicesPagingAndClamp(t *testing.T) {
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var invoices []domain.Invoice
	for i := 0; i < 5; i++ {
		invoices = append(invoices, domain.Invoice{
			GatewayID: string(rune('a' + i)),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	page := domain.FilterInvoices(invoices, base.Add(-time.Hour), base.Add(time.Hour), 2, 2)
	require.Len(t, page, 2)
	assert.Equal(t, "c", page[0].GatewayID)
	assert.Equal(t, "d", page[1].GatewayID)

	// A zero take applies the default; an oversized take is clamped.
	assert.Len(t, domain.FilterInvoices(invoices, base.Add(-time.Hour), base.Add(time.Hour), 0, 0), 5)

	var many []domain.Invoice
	for i := 0; i < domain.ExportTakeMax+100; i++ {
		many = append(many, domain.Invoice{
			GatewayID: "id",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	clamped := domain.FilterInvoices(many, base.Add(-time.Hour), base.Add(time.Hour), 0, 10_000)
	assert.Len(t, clamped, domain.ExportTakeMax)
}
