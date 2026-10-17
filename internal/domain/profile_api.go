package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cbl/cbl-kit/money"
)

// profileInvoiceTTL sets the stand invoice lifetime of provider profiles:
// real providers set their own expiration; the stand uses a fixed 15 minutes.
const profileInvoiceTTL = 15 * time.Minute

// standStoreID sets the BTCPay store identifier of the stand responses.
const standStoreID = "mock-store"

// nowCreateRequest describes the NOWPayments invoice creation body.
type nowCreateRequest struct {
	OrderID          string `json:"order_id"`
	PriceAmount      any    `json:"price_amount"`
	PriceCurrency    string `json:"price_currency"`
	PayCurrency      string `json:"pay_currency"`
	IPNCallbackURL   string `json:"ipn_callback_url"`
	OrderDescription string `json:"order_description"`
	SuccessURL       string `json:"success_url"`
	CancelURL        string `json:"cancel_url"`
}

// coinGateCreateRequest describes the CoinGate order creation body.
type coinGateCreateRequest struct {
	OrderID       string `json:"order_id"`
	PriceAmount   string `json:"price_amount"`
	PriceCurrency string `json:"price_currency"`
	PayCurrency   string `json:"pay_currency"`
	CallbackURL   string `json:"callback_url"`
	CancelURL     string `json:"cancel_url"`
}

// bitPayCreateRequest describes the BitPay invoice creation body.
type bitPayCreateRequest struct {
	Price           json.Number `json:"price"`
	Currency        string      `json:"currency"`
	Token           string      `json:"token"`
	OrderID         string      `json:"orderId"`
	NotificationURL string      `json:"notificationURL"`
	RedirectURL     string      `json:"redirectURL"`
	PosData         string      `json:"posData"`
}

// btcPayCreateRequest describes the BTCPay invoice creation body.
type btcPayCreateRequest struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
	Metadata struct {
		OrderID      string `json:"orderId"`
		ItemDesc     string `json:"itemDesc"`
		OrderNetwork string `json:"orderNetwork"`
	} `json:"metadata"`
	Checkout struct {
		RedirectURL string `json:"redirectURL"`
	} `json:"checkout"`
}

// ParseCreateInvoice parses the provider-shaped creation body into the platform
// request; now fixes the expiration (profileInvoiceTTL) and the order id serves
// as the idempotency key of the stand.
func (p Profile) ParseCreateInvoice(body []byte, now time.Time) (CreateInvoiceRequest, error) {
	request := CreateInvoiceRequest{ExpiresAt: now.UTC().Add(profileInvoiceTTL)}
	switch p.ID {
	case ProfileNOWPayments:
		var decoded nowCreateRequest
		if err := decodeStrictProfileJSON(body, &decoded); err != nil {
			return CreateInvoiceRequest{}, err
		}
		amount, err := ParseAssetAmount(decoded.PriceCurrency, decoded.PriceAmount)
		if err != nil {
			return CreateInvoiceRequest{}, err
		}
		request.PaymentID = decoded.OrderID
		request.Amount = amount
		request.Network = strings.ToLower(decoded.PayCurrency)
	case ProfileCoinGate:
		var decoded coinGateCreateRequest
		if err := decodeStrictProfileJSON(body, &decoded); err != nil {
			return CreateInvoiceRequest{}, err
		}
		amount, err := ParseAssetAmount(decoded.PriceCurrency, decoded.PriceAmount)
		if err != nil {
			return CreateInvoiceRequest{}, err
		}
		request.PaymentID = decoded.OrderID
		request.Amount = amount
		request.Network = strings.ToLower(decoded.PayCurrency)
	case ProfileBitPay:
		var decoded bitPayCreateRequest
		if err := decodeStrictProfileJSON(body, &decoded); err != nil {
			return CreateInvoiceRequest{}, err
		}
		// The price of the real API is a decimal number in major units of the
		// pricing currency; posData carries the requested network.
		amount, err := ParseAssetAmount(decoded.Currency, decoded.Price)
		if err != nil {
			return CreateInvoiceRequest{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
		request.PaymentID = decoded.OrderID
		request.Amount = amount
		request.Network = strings.ToLower(strings.TrimSpace(decoded.PosData))
		if request.Network == "" {
			request.Network = strings.ToLower(strings.TrimSpace(decoded.Currency))
		}
	case ProfileBTCPay:
		var decoded btcPayCreateRequest
		if err := decodeStrictProfileJSON(body, &decoded); err != nil {
			return CreateInvoiceRequest{}, err
		}
		amount, err := ParseAssetAmount(decoded.Currency, decoded.Amount)
		if err != nil {
			return CreateInvoiceRequest{}, err
		}
		request.PaymentID = decoded.Metadata.OrderID
		request.Amount = amount
		request.Network = strings.ToLower(decoded.Metadata.OrderNetwork)
		if request.Network == "" {
			request.Network = strings.ToLower(decoded.Currency)
		}
	default:
		return CreateInvoiceRequest{}, fmt.Errorf("%w: provider %q has no profile API", ErrInvalidRequest, p.ID)
	}

	if _, err := uuid.Parse(strings.TrimSpace(request.PaymentID)); err != nil {
		return CreateInvoiceRequest{}, fmt.Errorf("%w: order id must be uuid", ErrInvalidRequest)
	}
	if request.Amount.MinorUnits <= 0 {
		return CreateInvoiceRequest{}, fmt.Errorf("%w: amount must be positive", ErrInvalidRequest)
	}
	if !validCode(request.Amount.Asset, minAssetLen, maxAssetLen) {
		return CreateInvoiceRequest{}, fmt.Errorf("%w: invalid amount asset", ErrInvalidRequest)
	}
	if !validCode(request.Network, 1, maxNetworkLen) {
		return CreateInvoiceRequest{}, fmt.Errorf("%w: invalid network", ErrInvalidRequest)
	}
	request.IdempotencyKey = request.PaymentID
	return request, nil
}

// decodeStrictProfileJSON parses the body rejecting unknown fields, float
// numbers and trailing data; amounts are decoded as json.Number.
func decodeStrictProfileJSON(body []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected trailing json", ErrInvalidRequest)
	}
	return nil
}

// checkoutURL builds the stand-in hosted page URL of a profile invoice.
func (p Profile) checkoutURL(gatewayID string) string {
	return fmt.Sprintf("https://mock-provider.local/%s/checkout/%s", p.ID, gatewayID)
}

// nowInvoiceResponse describes the NOWPayments invoice body.
type nowInvoiceResponse struct {
	ID                     string          `json:"id"`
	OrderID                string          `json:"order_id"`
	PriceAmount            json.RawMessage `json:"price_amount"`
	PriceCurrency          string          `json:"price_currency"`
	PayCurrency            string          `json:"pay_currency"`
	InvoiceURL             string          `json:"invoice_url"`
	CreatedAt              string          `json:"created_at"`
	ExpirationEstimateDate string          `json:"expiration_estimate_date"`
}

// nowPaymentResponse describes the NOWPayments payment body.
type nowPaymentResponse struct {
	PaymentID     string          `json:"payment_id"`
	InvoiceID     string          `json:"invoice_id"`
	OrderID       string          `json:"order_id"`
	PaymentStatus string          `json:"payment_status"`
	PriceAmount   json.RawMessage `json:"price_amount"`
	ActuallyPaid  json.RawMessage `json:"actually_paid"`
	PayCurrency   string          `json:"pay_currency"`
	UpdatedAt     string          `json:"updated_at"`
}

// coinGateOrderResponse describes the CoinGate order body.
type coinGateOrderResponse struct {
	ID             json.Number `json:"id"`
	Status         string      `json:"status"`
	OrderID        string      `json:"order_id"`
	PriceAmount    string      `json:"price_amount"`
	PriceCurrency  string      `json:"price_currency"`
	PayAmount      string      `json:"pay_amount"`
	PayCurrency    string      `json:"pay_currency"`
	PaymentAddress string      `json:"payment_address"`
	CreatedAt      string      `json:"created_at"`
}

// bitPayInvoiceResponse describes the BitPay invoice body; the real API
// carries decimal major-unit amounts and wraps the body in a facade envelope.
type bitPayInvoiceResponse struct {
	ID             string          `json:"id"`
	URL            string          `json:"url"`
	Status         string          `json:"status"`
	Price          json.RawMessage `json:"price"`
	Currency       string          `json:"currency"`
	OrderID        string          `json:"orderId"`
	AmountPaid     json.RawMessage `json:"amountPaid"`
	Confirmations  uint32          `json:"confirmations"`
	InvoiceTime    int64           `json:"invoiceTime"`
	ExpirationTime int64           `json:"expirationTime"`
	BitcoinAddress string          `json:"bitcoinAddress"`
}

// bitPayEnvelope wraps an invoice body in the facade envelope of the real API.
type bitPayEnvelope struct {
	Facade string                `json:"facade"`
	Data   bitPayInvoiceResponse `json:"data"`
}

// btcPayInvoiceResponse describes the BTCPay invoice body.
type btcPayInvoiceResponse struct {
	ID             string            `json:"id"`
	StoreID        string            `json:"storeId"`
	Amount         string            `json:"amount"`
	Currency       string            `json:"currency"`
	Status         string            `json:"status"`
	CheckoutLink   string            `json:"checkoutLink"`
	CreatedTime    int64             `json:"createdTime"`
	ExpirationTime int64             `json:"expirationTime"`
	Metadata       map[string]string `json:"metadata"`
}

// rawStatusOf returns the raw status of the invoice or the canonical one.
func rawStatusOf(invoice Invoice) string {
	if invoice.RawStatus != "" {
		return invoice.RawStatus
	}
	return string(invoice.Status)
}

// eventTimeOf returns the invoice event time or its creation time.
func eventTimeOf(invoice Invoice) time.Time {
	if !invoice.EventTS.IsZero() {
		return invoice.EventTS.UTC()
	}
	return invoice.CreatedAt.UTC()
}

// amountLiteral renders an amount as a JSON number literal without float.
func amountLiteral(amount money.Money) json.RawMessage {
	return json.RawMessage(amount.String())
}

// CreateInvoiceResponse maps an invoice into the provider-shaped creation body.
func (p Profile) CreateInvoiceResponse(invoice Invoice) (any, error) {
	switch p.ID {
	case ProfileNOWPayments:
		return nowInvoiceResponse{
			ID:                     invoice.GatewayID,
			OrderID:                invoice.PaymentID,
			PriceAmount:            amountLiteral(invoice.Amount),
			PriceCurrency:          invoice.Amount.Asset,
			PayCurrency:            invoice.Network,
			InvoiceURL:             p.checkoutURL(invoice.GatewayID),
			CreatedAt:              invoice.CreatedAt.UTC().Format(time.RFC3339),
			ExpirationEstimateDate: invoice.ExpiresAt.UTC().Format(time.RFC3339),
		}, nil
	case ProfileCoinGate:
		return coinGateOrderResponse{
			ID:             json.Number(invoice.GatewayID),
			Status:         rawStatusOf(invoice),
			OrderID:        invoice.PaymentID,
			PriceAmount:    invoice.Amount.String(),
			PriceCurrency:  invoice.Amount.Asset,
			PayAmount:      invoice.Amount.String(),
			PayCurrency:    invoice.Network,
			PaymentAddress: invoice.Address,
			CreatedAt:      invoice.CreatedAt.UTC().Format(time.RFC3339),
		}, nil
	case ProfileBitPay:
		return bitPayEnvelope{Facade: "merchant/invoice", Data: bitPayInvoiceResponse{
			ID:             invoice.GatewayID,
			URL:            p.checkoutURL(invoice.GatewayID),
			Status:         rawStatusOf(invoice),
			Price:          amountLiteral(invoice.Amount),
			Currency:       invoice.Amount.Asset,
			OrderID:        invoice.PaymentID,
			AmountPaid:     amountLiteral(invoice.ReceivedAmount),
			Confirmations:  invoice.Confirmations,
			InvoiceTime:    invoice.CreatedAt.UnixMilli(),
			ExpirationTime: invoice.ExpiresAt.UnixMilli(),
			BitcoinAddress: invoice.Address,
		}}, nil
	case ProfileBTCPay:
		return btcPayInvoiceResponse{
			ID:             invoice.GatewayID,
			StoreID:        standStoreID,
			Amount:         invoice.Amount.String(),
			Currency:       invoice.Amount.Asset,
			Status:         rawStatusOf(invoice),
			CheckoutLink:   p.checkoutURL(invoice.GatewayID),
			CreatedTime:    invoice.CreatedAt.Unix(),
			ExpirationTime: invoice.ExpiresAt.Unix(),
			Metadata:       map[string]string{"orderId": invoice.PaymentID, "orderNetwork": invoice.Network},
		}, nil
	default:
		return nil, fmt.Errorf("%w: provider %q has no profile API", ErrInvalidRequest, p.ID)
	}
}

// PaymentResponse maps an invoice into the provider-shaped payment body.
func (p Profile) PaymentResponse(invoice Invoice) (any, error) {
	switch p.ID {
	case ProfileNOWPayments:
		return nowPaymentResponse{
			PaymentID:     invoice.GatewayID,
			InvoiceID:     invoice.GatewayID,
			OrderID:       invoice.PaymentID,
			PaymentStatus: rawStatusOf(invoice),
			PriceAmount:   amountLiteral(invoice.Amount),
			ActuallyPaid:  amountLiteral(invoice.ReceivedAmount),
			PayCurrency:   invoice.Network,
			UpdatedAt:     eventTimeOf(invoice).Format(time.RFC3339),
		}, nil
	case ProfileCoinGate:
		received := invoice.ReceivedAmount
		if received.MinorUnits == 0 {
			received = invoice.Amount
		}
		return coinGateOrderResponse{
			ID:             json.Number(invoice.GatewayID),
			Status:         rawStatusOf(invoice),
			OrderID:        invoice.PaymentID,
			PriceAmount:    invoice.Amount.String(),
			PriceCurrency:  invoice.Amount.Asset,
			PayAmount:      received.String(),
			PayCurrency:    invoice.Network,
			PaymentAddress: invoice.Address,
			CreatedAt:      invoice.CreatedAt.UTC().Format(time.RFC3339),
		}, nil
	case ProfileBitPay:
		received := invoice.ReceivedAmount
		if received.MinorUnits == 0 {
			received = invoice.Amount
		}
		return bitPayEnvelope{Facade: "merchant/invoice", Data: bitPayInvoiceResponse{
			ID:             invoice.GatewayID,
			URL:            p.checkoutURL(invoice.GatewayID),
			Status:         rawStatusOf(invoice),
			Price:          amountLiteral(invoice.Amount),
			Currency:       invoice.Amount.Asset,
			OrderID:        invoice.PaymentID,
			AmountPaid:     amountLiteral(received),
			Confirmations:  invoice.Confirmations,
			InvoiceTime:    invoice.CreatedAt.UnixMilli(),
			ExpirationTime: invoice.ExpiresAt.UnixMilli(),
			BitcoinAddress: invoice.Address,
		}}, nil
	case ProfileBTCPay:
		return btcPayInvoiceResponse{
			ID:             invoice.GatewayID,
			StoreID:        standStoreID,
			Amount:         invoice.Amount.String(),
			Currency:       invoice.Amount.Asset,
			Status:         rawStatusOf(invoice),
			CheckoutLink:   p.checkoutURL(invoice.GatewayID),
			CreatedTime:    invoice.CreatedAt.Unix(),
			ExpirationTime: invoice.ExpiresAt.Unix(),
			Metadata:       map[string]string{"orderId": invoice.PaymentID, "orderNetwork": invoice.Network},
		}, nil
	default:
		return nil, fmt.Errorf("%w: provider %q has no profile API", ErrInvalidRequest, p.ID)
	}
}
