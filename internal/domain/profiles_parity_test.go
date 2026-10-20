package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
)

// TestConnectorProvidersAreEmulated keeps the mock profile catalogue in sync
// with the connector manifests (CBL-17.22): every real provider has a profile.
// A new connector provider must add its profile and this row.
func TestConnectorProvidersAreEmulated(t *testing.T) {
	providers := []string{"nowpayments", "coingate", "bitpay", "btcpay"}
	for _, id := range providers {
		_, ok := domain.ProfileByID(id)
		assert.True(t, ok, "provider %q has no mock profile", id)
	}
	assert.Len(t, domain.AllProfiles(), len(providers)+1, "the mock profile plus every connector provider")
}
