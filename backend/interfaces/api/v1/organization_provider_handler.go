package v1

import (
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/agopalakrishnan/teams360/backend/application/services"
	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
	"github.com/agopalakrishnan/teams360/backend/infrastructure/dataprovider"
	"github.com/agopalakrishnan/teams360/backend/interfaces/dto"
	"github.com/agopalakrishnan/teams360/backend/interfaces/middleware"
	"github.com/agopalakrishnan/teams360/backend/pkg/logger"
	"github.com/gin-gonic/gin"
)

// OrganizationProviderHandler serves the external organization-data provider
// configuration status and the manual synchronization trigger. It holds no
// credential and performs no persistence or mapping of its own -- both live
// in OrganizationSyncService and OrganizationProviderRepository respectively.
type OrganizationProviderHandler struct {
	syncService  *services.OrganizationSyncService
	providerName string
}

// NewOrganizationProviderHandler creates the handler.
func NewOrganizationProviderHandler(syncService *services.OrganizationSyncService) *OrganizationProviderHandler {
	return &OrganizationProviderHandler{
		syncService:  syncService,
		providerName: "data-provider",
	}
}

// GetSettings handles GET /api/v1/admin/settings/organization-provider.
//
// This reports environment-configuration readiness only. It never queries a
// credential table (there is none) and never returns a token value.
func (h *OrganizationProviderHandler) GetSettings(c *gin.Context) {
	settings := dto.OrganizationProviderSettingsDTO{
		Provider:          h.providerName,
		BaseURLConfigured: os.Getenv(dataprovider.EnvBaseURL) != "",
		TokenConfigured:   os.Getenv(dataprovider.EnvAPIToken) != "",
		ReadyToSync:       h.syncService != nil && h.syncService.Configured(),
	}

	c.JSON(http.StatusOK, settings)
}

// Sync handles POST /api/v1/admin/organization-provider/sync.

func (h *OrganizationProviderHandler) Sync(c *gin.Context) {
	if h.syncService == nil || !h.syncService.Configured() {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{
			Error:   "Organization provider is not configured",
			Message: "Set " + dataprovider.EnvBaseURL + " and " + dataprovider.EnvAPIToken + " on the API service",
		})
		return
	}

	var request dto.OrganizationSyncRequestDTO
	if c.Request.Body != nil {
		if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, dto.ErrorResponse{
				Error:   "Invalid request body",
				Message: `Expected an empty body or {"overrideMassDeletion": true}`,
			})
			return
		}
	}

	actorID, _ := middleware.GetUserIDFromContext(c)

	if request.OverrideMassDeletion && !isAdminRequest(c) {
		logger.Get().Security("org_sync_mass_deletion_override_denied").
			UserID(actorID).
			IP(c.ClientIP()).
			Endpoint(c.Request.URL.Path).
			Details("non-admin requested a mass-deletion override").
			Log()
		c.JSON(http.StatusForbidden, dto.ErrorResponse{
			Error:   "Access denied: admin privileges required",
			Message: "Overriding the mass-deletion hold requires an administrator",
		})
		return
	}

	result, err := h.syncService.SyncWithOptions(c.Request.Context(), services.SyncOptions{
		OverrideMassDeletion: request.OverrideMassDeletion,
		ActorUserID:          actorID,
	})
	if err != nil {
		status, response := mapSyncError(err)
		c.JSON(status, response)
		return
	}

	c.JSON(http.StatusOK, result)
}

// isAdminRequest re-derives admin status from the validated JWT claims already
// on the context. It mirrors AdminOnlyMiddleware's rule rather than inventing a
// second one, and exists so the override can never be authorized by anything
// the client sent in the request body.
func isAdminRequest(c *gin.Context) bool {
	level, exists := c.Get("hierarchyLevel")
	if !exists {
		return false
	}
	asString, ok := level.(string)
	return ok && (asString == "level-1" || asString == "level-admin")
}

// mapSyncError converts a sync failure into a status code and a safe body.
// None of these branches echo a token, header, or the raw provider payload --
// including the mass-deletion hold, which carries aggregate counts only.
func mapSyncError(err error) (int, any) {
	switch {
	case errors.Is(err, services.ErrSyncInProgress):
		return http.StatusConflict, dto.ErrorResponse{
			Error: "A synchronization is already running",
		}

	case errors.Is(err, services.ErrProviderNotConfigured):
		return http.StatusBadRequest, dto.ErrorResponse{
			Error:   "Organization provider is not configured",
			Message: "Set " + dataprovider.EnvBaseURL + " and " + dataprovider.EnvAPIToken + " on the API service",
		}

	case errors.Is(err, services.ErrInvalidSnapshot):
		return http.StatusBadGateway, dto.ErrorResponse{
			Error:   "Provider returned data that failed contract validation",
			Message: err.Error(),
		}

	case errors.Is(err, services.ErrProviderFetchFailed):
		// The provider failed or returned an unusable response, so return 502 for this upstream failure.
		return http.StatusBadGateway, dto.ErrorResponse{
			Error:   "Failed to reach the organization data provider",
			Message: err.Error(),
		}

	case errors.Is(err, orgprovider.ErrMassDeletionBlocked):
		response := dto.MassDeletionHoldResponseDTO{
			ErrorResponse: dto.ErrorResponse{
				Error:   "Synchronization held for review",
				Message: "This sync would remove an unusually large share of users or teams. No changes were made. Review the incoming provider data, then either fix it upstream or re-run the sync with an explicit override.",
				Code:    dto.CodeMassDeletionHold,
			},
			Applied: false,
		}
		var hold *orgprovider.MassDeletionHoldError
		if errors.As(err, &hold) {
			response.MassDeletion = massDeletionReportDTO(hold.Report)
		}
		return http.StatusConflict, response

	default:
		// Service-side failures are internal errors, so return 500 rather than 502.
		logger.Get().WithError(err).Error("organization provider sync failed")
		return http.StatusInternalServerError, dto.ErrorResponse{
			Error:   "Synchronization failed",
			Message: err.Error(),
		}
	}
}

// massDeletionReportDTO maps the domain report onto the transport shape. The
// dto package imports no domain package, so the translation lives here.
func massDeletionReportDTO(report orgprovider.MassDeletionReport) *dto.MassDeletionReportDTO {
	out := &dto.MassDeletionReportDTO{
		Threshold: report.Threshold,
		Users:     deletionMetricDTO(report.Users),
		Teams:     deletionMetricDTO(report.Teams),
	}
	if report.Memberships != nil {
		memberships := deletionMetricDTO(*report.Memberships)
		out.Memberships = &memberships
	}
	return out
}

func deletionMetricDTO(metric orgprovider.DeletionMetric) dto.DeletionMetricDTO {
	return dto.DeletionMetricDTO{
		Kind:              metric.Kind,
		Existing:          metric.Existing,
		Incoming:          metric.Incoming,
		Deleting:          metric.Deleting,
		Percent:           metric.Percent,
		Threshold:         metric.Threshold,
		ExceedsThreshold:  metric.ExceedsThreshold,
		ContributesToHold: metric.ContributesToHold,
	}
}
