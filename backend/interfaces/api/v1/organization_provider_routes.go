package v1

import (
	"github.com/agopalakrishnan/teams360/backend/application/services"
	"github.com/agopalakrishnan/teams360/backend/domain/orgprovider"
	"github.com/agopalakrishnan/teams360/backend/interfaces/middleware"
	"github.com/gin-gonic/gin"
)

// SetupOrganizationProviderRoutes configures the external organization-data
// provider routes. Like every other admin route, these require a valid JWT and
// admin privileges.
//
// The settings endpoints join the existing /api/v1/admin/settings group rather
// than introducing a second configuration surface.
func SetupOrganizationProviderRoutes(
	router *gin.Engine,
	repo orgprovider.Repository,
	syncService *services.OrganizationSyncService,
	encrypter TokenEncrypter,
	jwtService *services.JWTService,
) {
	handler := NewOrganizationProviderHandler(repo, syncService, encrypter)

	admin := router.Group("/api/v1/admin")
	admin.Use(middleware.JWTAuthMiddleware(jwtService))
	admin.Use(middleware.AdminOnlyMiddleware())
	{
		admin.GET("/settings/organization-provider", handler.GetSettings)
		admin.PUT("/settings/organization-provider", handler.UpdateToken)

		// Manual trigger. There is deliberately no scheduler: a sync rewrites
		// org structure, so a human decides when it happens.
		admin.POST("/organization-provider/sync", handler.Sync)
	}
}
