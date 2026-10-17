package domain

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cbl/cbl-kit/money"
)

// ProfileID identifies an emulated provider.
type ProfileID string

// Emulated provider profiles.
const (
	// ProfileMock is the platform sandbox provider (CBL-3.5 contract).
	ProfileMock ProfileID = "mock"
	// ProfileNOWPayments emulates NOWPayments.
	ProfileNOWPayments ProfileID = "nowpayments"
	// ProfileCoinGate emulates CoinGate.
	ProfileCoinGate ProfileID = "coingate"
	// ProfileBitPay emulates BitPay.
	ProfileBitPay ProfileID = "bitpay"
	// ProfileBTCPay emulates BTCPay Server.
	ProfileBTCPay ProfileID = "btcpay"
)

// SignatureKind selects the webhook signing scheme of a profile.
type SignatureKind string

// Webhook signature kinds.
const (
	// SignatureLegacyHMAC is the platform t/v1 scheme of the kit hmac package (mock).
	SignatureLegacyHMAC SignatureKind = "legacy_hmac"
	// SignatureRawHMAC is HMAC over the raw body (BitPay base64, BTCPay hex with prefix).
	SignatureRawHMAC SignatureKind = "raw_hmac"
	// SignatureCanonicalJSONHMAC is HMAC-SHA512 over the canonical JSON body (NOWPayments).
	SignatureCanonicalJSONHMAC SignatureKind = "canonical_json_hmac"
	// SignatureTokenEcho is the token echo body field (CoinGate).
	SignatureTokenEcho SignatureKind = "token_echo"
)

// SignatureSpec describes how a profile signs its webhooks.
type SignatureSpec struct {
	// Kind selects the signing scheme.
	Kind SignatureKind
	// Header sets the signature header of raw_hmac, canonical_json_hmac and legacy_hmac.
	Header string
	// Encoding sets the raw_hmac digest encoding: hex or base64.
	Encoding string
	// Prefix sets an optional digest prefix, for example "sha256=" of BTCPay.
	Prefix string
	// Field sets the token echo body field (token_echo).
	Field string
}

// Profile describes one emulated provider: the wire contract of section 3.2 of
// the CBL-17.5 spec (fields, statuses, auth and signatures).
type Profile struct {
	// ID sets the provider identifier.
	ID ProfileID
	// DisplayName sets the operator-facing name.
	DisplayName string
	// AuthHeader sets the required non-empty request header.
	AuthHeader string
	// AuthPrefix sets an optional required value prefix, for example "Bearer ".
	AuthPrefix string
	// StatusMap maps canonical simulate statuses to raw provider statuses.
	StatusMap map[Status]string
	// RawStatuses lists the documented raw statuses of the provider.
	RawStatuses []string
	// Signature describes the webhook signing scheme.
	Signature SignatureSpec
}

// builtinProfiles returns the profile catalogue in a stable order.
func builtinProfiles() []Profile {
	return []Profile{
		{
			ID:          ProfileMock,
			DisplayName: "Mock gateway (dev)",
			StatusMap: map[Status]string{
				StatusPending:   string(StatusPending),
				StatusDetected:  string(StatusDetected),
				StatusConfirmed: string(StatusConfirmed),
				StatusFailed:    string(StatusFailed),
				StatusExpired:   string(StatusExpired),
				StatusUnderpaid: string(StatusUnderpaid),
				StatusOverpaid:  string(StatusOverpaid),
			},
			RawStatuses: []string{
				string(StatusPending), string(StatusDetected), string(StatusConfirmed),
				string(StatusFailed), string(StatusExpired), string(StatusUnderpaid), string(StatusOverpaid),
			},
			Signature: SignatureSpec{Kind: SignatureLegacyHMAC, Header: "X-Billing-Signature"},
		},
		{
			ID:          ProfileNOWPayments,
			DisplayName: "NOWPayments",
			AuthHeader:  "x-api-key",
			StatusMap: map[Status]string{
				StatusPending:   "waiting",
				StatusDetected:  "confirming",
				StatusConfirmed: "finished",
				StatusFailed:    "failed",
				StatusExpired:   "expired",
				StatusUnderpaid: "partially_paid",
				StatusOverpaid:  "finished",
			},
			RawStatuses: []string{"waiting", "confirming", "confirmed", "sending", "partially_paid", "finished", "failed", "refunded", "expired"},
			Signature:   SignatureSpec{Kind: SignatureCanonicalJSONHMAC, Header: "x-nowpayments-sig", Encoding: "hex"},
		},
		{
			ID:          ProfileCoinGate,
			DisplayName: "CoinGate",
			AuthHeader:  "Authorization",
			AuthPrefix:  "Bearer ",
			StatusMap: map[Status]string{
				StatusPending:   "new",
				StatusDetected:  "confirming",
				StatusConfirmed: "paid",
				StatusFailed:    "invalid",
				StatusExpired:   "expired",
				StatusUnderpaid: "invalid",
				StatusOverpaid:  "paid",
			},
			RawStatuses: []string{"new", "pending", "confirming", "paid", "invalid", "expired", "canceled", "refunded"},
			Signature:   SignatureSpec{Kind: SignatureTokenEcho, Field: "token"},
		},
		{
			ID:          ProfileBitPay,
			DisplayName: "BitPay",
			AuthHeader:  "X-Identity",
			StatusMap: map[Status]string{
				StatusPending:   "new",
				StatusDetected:  "paid",
				StatusConfirmed: "complete",
				StatusFailed:    "invalid",
				StatusExpired:   "expired",
				StatusUnderpaid: "paid",
				StatusOverpaid:  "paid",
			},
			RawStatuses: []string{"new", "paid", "confirmed", "complete", "expired", "invalid"},
			Signature:   SignatureSpec{Kind: SignatureRawHMAC, Header: "x-signature", Encoding: "base64"},
		},
		{
			ID:          ProfileBTCPay,
			DisplayName: "BTCPay Server",
			AuthHeader:  "Authorization",
			AuthPrefix:  "token ",
			StatusMap: map[Status]string{
				StatusPending:   "New",
				StatusDetected:  "Processing",
				StatusConfirmed: "Settled",
				StatusFailed:    "Invalid",
				StatusExpired:   "Expired",
				StatusUnderpaid: "Processing",
				StatusOverpaid:  "Settled",
			},
			RawStatuses: []string{"New", "Processing", "Settled", "Expired", "Invalid"},
			Signature:   SignatureSpec{Kind: SignatureRawHMAC, Header: "BTCPay-Sig", Encoding: "hex", Prefix: "sha256="},
		},
	}
}

// ProfileByID returns the profile by its identifier.
func ProfileByID(id string) (Profile, bool) {
	for _, profile := range builtinProfiles() {
		if string(profile.ID) == id {
			return profile, true
		}
	}
	return Profile{}, false
}

// AllProfiles returns all profiles in a stable order.
func AllProfiles() []Profile {
	return builtinProfiles()
}

// RawStatus maps a canonical simulate status to the provider raw status.
func (p Profile) RawStatus(canonical Status) (string, error) {
	raw, ok := p.StatusMap[canonical]
	if !ok || raw == "" {
		return "", fmt.Errorf("%w: provider %q does not support status %q", ErrInvalidRequest, p.ID, canonical)
	}
	return raw, nil
}

// HasRawStatus reports whether the raw status is in the documented list.
func (p Profile) HasRawStatus(raw string) bool {
	return slices.Contains(p.RawStatuses, raw)
}

// ValidateAuth checks the profile auth requirements: header presence, the value
// prefix and, for BitPay, the base64 shape of the request signature.
func (p Profile) ValidateAuth(header http.Header) error {
	if p.ID == ProfileBitPay {
		identity := strings.TrimSpace(header.Get("X-Identity"))
		signature := strings.TrimSpace(header.Get("X-Signature"))
		if identity == "" || signature == "" {
			return fmt.Errorf("%w: missing X-Identity or X-Signature", ErrUnauthorized)
		}
		if _, err := base64.StdEncoding.DecodeString(signature); err != nil {
			return fmt.Errorf("%w: X-Signature is not base64", ErrUnauthorized)
		}
		return nil
	}
	value := strings.TrimSpace(header.Get(p.AuthHeader))
	if value == "" {
		return fmt.Errorf("%w: missing %s", ErrUnauthorized, p.AuthHeader)
	}
	if p.AuthPrefix != "" {
		if !strings.HasPrefix(value, p.AuthPrefix) {
			return fmt.Errorf("%w: %s misses the %q prefix", ErrUnauthorized, p.AuthHeader, p.AuthPrefix)
		}
		if strings.TrimSpace(strings.TrimPrefix(value, p.AuthPrefix)) == "" {
			return fmt.Errorf("%w: empty %s value", ErrUnauthorized, p.AuthHeader)
		}
	}
	return nil
}

// AssetDecimals returns the decimals of an asset in the stand table
// (spec section 3.2): known crypto assets plus a fiat-like default of 2.
func AssetDecimals(asset string) uint32 {
	switch strings.ToUpper(strings.TrimSpace(asset)) {
	case "USDT", "USDC", "TRX":
		return 6
	case "BTC", "LTC":
		return 8
	case "ETH":
		return 18
	default:
		return 2
	}
}

// ParseAssetAmount parses a provider amount (a decimal string or a JSON number)
// into Money of the asset with the stand decimals table, without float.
func ParseAssetAmount(asset string, value any) (money.Money, error) {
	decimals := AssetDecimals(asset)
	amount, err := money.ParseAny(value, strings.ToUpper(strings.TrimSpace(asset)), decimals)
	if err != nil {
		return money.Money{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return amount, nil
}
