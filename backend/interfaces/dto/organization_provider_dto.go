package dto

import "time"

// OrganizationProviderSettingsDTO reports how the external organization-data
// provider is configured.
//
// It never carries the API token, encrypted or otherwise: the token leaves the
// database only to be sent to the provider. The UI needs to know whether a token
// exists, not what it is.
type OrganizationProviderSettingsDTO struct {
	Provider string `json:"provider"`

	// Configured is true when a token has been stored.
	Configured bool `json:"configured"`
	// BaseURLConfigured is true when the provider URL is set in the environment.
	BaseURLConfigured bool `json:"baseUrlConfigured"`
	// EncryptionConfigured is true when a token encryption key is available.
	EncryptionConfigured bool `json:"encryptionConfigured"`
	// ReadyToSync is true when every prerequisite above is met.
	ReadyToSync bool `json:"readyToSync"`

	TokenUpdatedAt *time.Time `json:"tokenUpdatedAt,omitempty"`
}

// UpdateOrganizationProviderTokenRequest carries a new provider API token.
// This field is write-only; no endpoint ever returns it.
type UpdateOrganizationProviderTokenRequest struct {
	APIToken string `json:"apiToken" binding:"required"`
}
