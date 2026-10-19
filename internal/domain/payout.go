package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cbl/cbl-kit/money"
)

// Payout state words of the Greenfield payout flow; the vocabulary is the
// connector payoutStates table (17.9/17.11), so the stand never invents words
// the platform cannot map.
const (
	// PayoutStateAwaitingApproval sets the initial state of a stand payout.
	PayoutStateAwaitingApproval = "AwaitingApproval"
	// PayoutStateAwaitingPayment sets the awaiting-payment provider state.
	PayoutStateAwaitingPayment = "AwaitingPayment"
	// PayoutStateInProgress sets the in-progress provider state.
	PayoutStateInProgress = "InProgress"
	// PayoutStateCompleted sets the completed provider state.
	PayoutStateCompleted = "Completed"
	// PayoutStateCancelled sets the cancelled provider state.
	PayoutStateCancelled = "Cancelled"
)

// Export paging bounds of the store invoice list: the connector transport
// bounds the export limit at 500, the stand clamps to the same value.
const (
	// ExportTakeDefault sets the page size of a list request without take.
	ExportTakeDefault = 100
	// ExportTakeMax sets the maximum page size of a list request.
	ExportTakeMax = 500
)

// Domain errors of the payout and export emulation.
var (
	// ErrPullPaymentNotFound reports an unknown pull payment.
	ErrPullPaymentNotFound = errors.New("mock-gateway: pull payment not found")
	// ErrPayoutNotFound reports an unknown payout.
	ErrPayoutNotFound = errors.New("mock-gateway: payout not found")
)

// PullPayment describes a provider pull payment (BTCPay Greenfield): the funds
// holder of the two-step payout flow; a refund reuses the same flow.
type PullPayment struct {
	// ID sets the provider pull payment identifier.
	ID string
	// Name sets the operator-facing name; the connector sends the platform
	// payout id or refund-<id> and the stand replays by it (T-MOCK-09).
	Name string
	// Amount sets the held amount.
	Amount money.Money
	// PayoutMethods lists the allowed payout method codes.
	PayoutMethods []string
	// CreatedAt sets the creation time.
	CreatedAt time.Time
}

// Payout describes a provider payout; a refund is a payout in the stand
// vocabulary (the connector aliases the refund read to the payout read).
type Payout struct {
	// ID sets the provider payout identifier.
	ID string
	// PullPaymentID sets the holding pull payment.
	PullPaymentID string
	// Destination sets the payout destination address.
	Destination string
	// Amount sets the payout amount.
	Amount money.Money
	// MethodID sets the provider payout method (for example BTC-CHAIN).
	MethodID string
	// State sets the provider state word.
	State string
	// CreatedAt sets the creation time.
	CreatedAt time.Time
}

// PullPaymentRequest describes a parsed pull payment creation body.
type PullPaymentRequest struct {
	// Name sets the pull payment name.
	Name string
	// Amount sets the held amount.
	Amount money.Money
	// PayoutMethods lists the allowed payout method codes.
	PayoutMethods []string
}

// PayoutRequest describes a parsed payout creation body.
type PayoutRequest struct {
	// Destination sets the payout destination address.
	Destination string
	// Amount sets the payout amount.
	Amount money.Money
	// MethodID sets the provider payout method.
	MethodID string
}

// ValidPayoutState reports whether the word is a known provider payout state.
func ValidPayoutState(state string) bool {
	switch state {
	case PayoutStateAwaitingApproval, PayoutStateAwaitingPayment, PayoutStateInProgress,
		PayoutStateCompleted, PayoutStateCancelled:
		return true
	default:
		return false
	}
}

// Validate checks the pull payment request fields; the payout methods are
// bounded (maxPayoutMethods) so a body-sized array cannot amplify the store
// memory beyond the record cap (security review CBL-17.27).
func (r PullPaymentRequest) Validate() error {
	if name := strings.TrimSpace(r.Name); name == "" || len(name) > maxNameLen {
		return fmt.Errorf("%w: invalid pull payment name", ErrInvalidRequest)
	}
	if _, err := money.New(r.Amount.MinorUnits, r.Amount.Asset, r.Amount.Decimals); err != nil {
		return fmt.Errorf("%w: amount: %w", ErrInvalidRequest, err)
	}
	if r.Amount.MinorUnits <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrInvalidRequest)
	}
	if !validCode(r.Amount.Asset, minAssetLen, maxAssetLen) {
		return fmt.Errorf("%w: invalid amount asset", ErrInvalidRequest)
	}
	if len(r.PayoutMethods) == 0 || len(r.PayoutMethods) > maxPayoutMethods {
		return fmt.Errorf("%w: payout methods out of bounds", ErrInvalidRequest)
	}
	for _, method := range r.PayoutMethods {
		if !validCode(strings.TrimSpace(method), minAssetLen, maxAssetLen) {
			return fmt.Errorf("%w: invalid payout method", ErrInvalidRequest)
		}
	}
	return nil
}

// Validate checks the payout request fields.
func (r PayoutRequest) Validate() error {
	if destination := strings.TrimSpace(r.Destination); destination == "" || len(destination) > maxAddressLen {
		return fmt.Errorf("%w: invalid payout destination", ErrInvalidRequest)
	}
	if _, err := money.New(r.Amount.MinorUnits, r.Amount.Asset, r.Amount.Decimals); err != nil {
		return fmt.Errorf("%w: amount: %w", ErrInvalidRequest, err)
	}
	if r.Amount.MinorUnits <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrInvalidRequest)
	}
	if !validCode(strings.TrimSpace(r.MethodID), minAssetLen, maxAssetLen+len("-CHAIN")) {
		return fmt.Errorf("%w: invalid payout method", ErrInvalidRequest)
	}
	return nil
}

// AllowsPayout verifies a payout against its pull payment: the method asset
// must be offered and the amount must not exceed the held amount, so the stand
// cannot hold inconsistent records the real provider would reject.
func (p PullPayment) AllowsPayout(req PayoutRequest) error {
	asset := strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(req.MethodID), "-CHAIN"))
	allowed := false
	for _, method := range p.PayoutMethods {
		if strings.EqualFold(strings.TrimSpace(method), asset) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: payout method %q is not offered by the pull payment", ErrInvalidRequest, req.MethodID)
	}
	if !strings.EqualFold(strings.TrimSpace(req.Amount.Asset), strings.TrimSpace(p.Amount.Asset)) ||
		req.Amount.MinorUnits > p.Amount.MinorUnits {
		return fmt.Errorf("%w: payout amount exceeds the pull payment", ErrInvalidRequest)
	}
	return nil
}

// SortInvoicesByCreated orders invoices by (created time at the wire
// granularity, gateway id): the deterministic order keeps the offset cursor
// walk of the connector export and the order lookup stable.
func SortInvoicesByCreated(invoices []Invoice) {
	sort.Slice(invoices, func(i, j int) bool {
		left, right := invoices[i].CreatedAt.UTC().Unix(), invoices[j].CreatedAt.UTC().Unix()
		if left != right {
			return left < right
		}
		return invoices[i].GatewayID < invoices[j].GatewayID
	})
}

// FilterInvoices returns the invoices of the inclusive window [from, to]
// compared at the wire granularity (unix seconds, matching createdTime),
// sorted deterministically by (created time, gateway id) and paged by
// skip/take; a non-positive take applies the default and an oversized one is
// clamped (ExportTakeMax).
func FilterInvoices(invoices []Invoice, from, to time.Time, skip, take int) []Invoice {
	if skip < 0 {
		skip = 0
	}
	if take <= 0 {
		take = ExportTakeDefault
	}
	if take > ExportTakeMax {
		take = ExportTakeMax
	}
	from, to = from.UTC().Truncate(time.Second), to.UTC().Truncate(time.Second)
	filtered := make([]Invoice, 0, len(invoices))
	for _, invoice := range invoices {
		created := invoice.CreatedAt.UTC().Truncate(time.Second)
		if created.Before(from) || created.After(to) {
			continue
		}
		filtered = append(filtered, invoice)
	}
	SortInvoicesByCreated(filtered)
	if skip >= len(filtered) {
		return []Invoice{}
	}
	filtered = filtered[skip:]
	if len(filtered) > take {
		filtered = filtered[:take]
	}
	return filtered
}
