package v1

import (
	"errors"
	"net/http"
	"strings"

	"github.com/agopalakrishnan/teams360/backend/application/services"
	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
	"github.com/agopalakrishnan/teams360/backend/infrastructure/provider/podiq"
	"github.com/agopalakrishnan/teams360/backend/interfaces/dto"
	"github.com/agopalakrishnan/teams360/backend/pkg/logger"
	"github.com/agopalakrishnan/teams360/backend/pkg/tokencrypto"
	"github.com/gin-gonic/gin"
)

// TokenEncrypter encrypts a provider token for storage.
type TokenEncrypter interface {
	Encrypt(plaintext string) (string, error)
}

// OrganizationProviderHandler serves the external organization-data provider
// configuration and the manual synchronization trigger.
type OrganizationProviderHandler struct {
	repo         orgprovider.Repository
	syncService  *services.OrganizationSyncService
	encrypter    TokenEncrypter
	providerName string
}

// NewOrganizationProviderHandler creates the handler.
func NewOrganizationProviderHandler(
	repo orgprovider.Repository,
	syncService *services.OrganizationSyncService,
	encrypter TokenEncrypter,
) *OrganizationProviderHandler {
	return &OrganizationProviderHandler{
		repo:         repo,
		syncService:  syncService,
		encrypter:    encrypter,
		providerName: "podiq",
	}
}

// GetSettings handles GET /api/v1/admin/settings/organization-provider.
func (h *OrganizationProviderHandler) GetSettings(c *gin.Context) {
	creds, err := h.repo.GetCredentials(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{
			Error:   "Failed to read provider settings",
			Message: err.Error(),
		})
		return
	}

	settings := dto.OrganizationProviderSettingsDTO{
		Provider:             h.providerName,
		BaseURLConfigured:    podiq.LoadConfig() != nil,
		EncryptionConfigured: h.encrypter != nil,
	}

	if creds != nil && creds.APITokenEncrypted != "" {
		settings.Configured = true
		settings.Provider = creds.Provider
		updatedAt := creds.UpdatedAt
		settings.TokenUpdatedAt = &updatedAt
	}

	settings.ReadyToSync = settings.Configured && settings.BaseURLConfigured && settings.EncryptionConfigured

	c.JSON(http.StatusOK, settings)
}

// UpdateToken handles PUT /api/v1/admin/settings/organization-provider.
func (h *OrganizationProviderHandler) UpdateToken(c *gin.Context) {
	var req dto.UpdateOrganizationProviderTokenRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Invalid request body", Message: err.Error()})
		return
	}

	token := strings.TrimSpace(req.APIToken)
	if token == "" {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "API token must not be empty"})
		return
	}

	if h.encrypter == nil {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{
			Error:   "Token encryption is not configured",
			Message: "Set " + tokencrypto.EnvKey + " to a base64-encoded 32-byte key before storing a provider token",
		})
		return
	}

	encrypted, err := h.encrypter.Encrypt(token)
	if err != nil {
		// Never echo err.Error() here — an encryption failure message could
		// describe the value being encrypted.
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{Error: "Failed to protect provider token"})
		return
	}

	if err := h.repo.SaveCredentials(c.Request.Context(), h.providerName, encrypted); err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{
			Error:   "Failed to save provider token",
			Message: err.Error(),
		})
		return
	}

	logger.Get().Info("organization provider token updated")
	dto.RespondMessage(c, http.StatusOK, "Provider token saved successfully")
}

// Sync handles POST /api/v1/admin/organization-provider/sync.
func (h *OrganizationProviderHandler) Sync(c *gin.Context) {
	if h.syncService == nil {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Organization provider sync is not configured"})
		return
	}

	result, err := h.syncService.Sync(c.Request.Context())
	if err != nil {
		status, response := mapSyncError(err)
		c.JSON(status, response)
		return
	}

	c.JSON(http.StatusOK, result)
}

// mapSyncError converts a sync failure into a status code and a safe body.
// Provider and encryption errors are already written to avoid disclosing the
// token or the upstream response, so their text can be surfaced to an admin.
func mapSyncError(err error) (int, dto.ErrorResponse) {
	switch {
	case errors.Is(err, services.ErrSyncInProgress):
		return http.StatusConflict, dto.ErrorResponse{
			Error: "A synchronization is already running",
		}

	case errors.Is(err, services.ErrProviderNotConfigured), errors.Is(err, podiq.ErrNotConfigured):
		return http.StatusBadRequest, dto.ErrorResponse{
			Error:   "Organization provider is not configured",
			Message: "Set " + podiq.EnvBaseURL + " and save a provider API token before syncing",
		}

	case errors.Is(err, services.ErrEncryptionNotConfigured), errors.Is(err, tokencrypto.ErrNotConfigured):
		return http.StatusBadRequest, dto.ErrorResponse{
			Error:   "Token encryption is not configured",
			Message: "Set " + tokencrypto.EnvKey + " to a base64-encoded 32-byte key",
		}

	case errors.Is(err, tokencrypto.ErrInvalidCiphertext):
		return http.StatusBadRequest, dto.ErrorResponse{
			Error:   "Stored provider token could not be decrypted",
			Message: "The encryption key may have changed. Save the provider token again.",
		}

	case errors.Is(err, services.ErrInvalidSnapshot):
		return http.StatusBadGateway, dto.ErrorResponse{
			Error:   "Provider returned data that failed contract validation",
			Message: err.Error(),
		}

	default:
		logger.Get().WithError(err).Error("organization provider sync failed")
		return http.StatusBadGateway, dto.ErrorResponse{
			Error:   "Synchronization failed",
			Message: err.Error(),
		}
	}
}
