package v1

import (
	"net/http"
	"os"

	"github.com/agopalakrishnan/teams360/backend/domain/organization"
	"github.com/agopalakrishnan/teams360/backend/interfaces/dto"
	"github.com/gin-gonic/gin"
)

// SettingsAdminHandler handles settings-related admin HTTP requests
type SettingsAdminHandler struct {
	orgRepo organization.Repository
}

// NewSettingsAdminHandler creates a new SettingsAdminHandler
func NewSettingsAdminHandler(orgRepo organization.Repository) *SettingsAdminHandler {
	return &SettingsAdminHandler{orgRepo: orgRepo}
}

// GetDimensions handles GET /api/v1/admin/settings/dimensions
//
// @Summary List health dimensions (admin)
// @Description Returns all health dimensions, including inactive ones. Requires admin (level-1) role.
// @Tags admin-settings
// @Produce json
// @Success 200 {object} dto.DimensionsResponse
// @Failure 500 {object} dto.ErrorResponse "Failed to query dimensions"
// @Security BearerAuth
// @Router /admin/settings/dimensions [get]
func (h *SettingsAdminHandler) GetDimensions(c *gin.Context) {
	dimensions, err := h.orgRepo.FindDimensions(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{
			Error:   "Failed to query dimensions",
			Message: err.Error(),
		})
		return
	}

	// Convert to DTOs
	dimensionDTOs := make([]dto.HealthDimensionDTO, len(dimensions))
	for i, dim := range dimensions {
		dimensionDTOs[i] = dto.HealthDimensionDTO{
			ID:              dim.ID,
			Name:            dim.Name,
			Description:     dim.Description,
			GoodDescription: dim.GoodDescription,
			BadDescription:  dim.BadDescription,
			IsActive:        dim.IsActive,
			Weight:          dim.Weight,
			CreatedAt:       dim.CreatedAt,
			UpdatedAt:       dim.UpdatedAt,
		}
	}

	c.JSON(http.StatusOK, dto.DimensionsResponse{Dimensions: dimensionDTOs})
}

// CreateDimension handles POST /api/v1/admin/settings/dimensions
//
// @Summary Create a health dimension
// @Description Creates a new health dimension. Weight must be between 0 and 10 (defaults to 1.0); isActive defaults to true. Requires admin (level-1) role.
// @Tags admin-settings
// @Accept json
// @Produce json
// @Param body body dto.CreateDimensionRequest true "Dimension to create"
// @Success 201 {object} dto.HealthDimensionDTO
// @Failure 400 {object} dto.ErrorResponse "Invalid request body or weight out of range"
// @Failure 500 {object} dto.ErrorResponse "Failed to create dimension"
// @Security BearerAuth
// @Router /admin/settings/dimensions [post]
func (h *SettingsAdminHandler) CreateDimension(c *gin.Context) {
	var req dto.CreateDimensionRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Invalid request body", Message: err.Error()})
		return
	}

	// Validate weight
	if req.Weight < 0 || req.Weight > 10 {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Weight must be between 0 and 10"})
		return
	}

	// Default isActive to true if not provided
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	// Default weight to 1.0 if not provided
	weight := req.Weight
	if weight == 0 {
		weight = 1.0
	}

	// Create domain object
	dim := &organization.HealthDimension{
		ID:              req.ID,
		Name:            req.Name,
		Description:     req.Description,
		GoodDescription: req.GoodDescription,
		BadDescription:  req.BadDescription,
		IsActive:        isActive,
		Weight:          weight,
	}

	// Save using repository
	if err := h.orgRepo.SaveDimension(c.Request.Context(), dim); err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{
			Error:   "Failed to create dimension",
			Message: err.Error(),
		})
		return
	}

	// Convert to DTO and return
	responseDTO := dto.HealthDimensionDTO{
		ID:              dim.ID,
		Name:            dim.Name,
		Description:     dim.Description,
		GoodDescription: dim.GoodDescription,
		BadDescription:  dim.BadDescription,
		IsActive:        dim.IsActive,
		Weight:          dim.Weight,
		CreatedAt:       dim.CreatedAt,
		UpdatedAt:       dim.UpdatedAt,
	}

	c.JSON(http.StatusCreated, responseDTO)
}

// UpdateDimension handles PUT /api/v1/admin/settings/dimensions/:id
//
// @Summary Update a health dimension
// @Description Updates a health dimension's fields. Weight must be between 0 and 10. Requires admin (level-1) role.
// @Tags admin-settings
// @Accept json
// @Produce json
// @Param id path string true "Dimension ID"
// @Param body body dto.UpdateDimensionRequest true "Fields to update"
// @Success 200 {object} dto.HealthDimensionDTO
// @Failure 400 {object} dto.ErrorResponse "Invalid request body or weight out of range"
// @Failure 404 {object} dto.ErrorResponse "Dimension not found"
// @Failure 500 {object} dto.ErrorResponse "Failed to update dimension"
// @Security BearerAuth
// @Router /admin/settings/dimensions/{id} [put]
func (h *SettingsAdminHandler) UpdateDimension(c *gin.Context) {
	id := c.Param("id")
	var req dto.UpdateDimensionRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Invalid request body", Message: err.Error()})
		return
	}

	// Validate weight if provided
	if req.Weight != nil && (*req.Weight < 0 || *req.Weight > 10) {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Weight must be between 0 and 10"})
		return
	}

	// Find existing dimension
	dim, err := h.orgRepo.FindDimensionByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, dto.ErrorResponse{Error: "Dimension not found"})
		return
	}

	// Update fields if provided
	if req.Name != nil {
		dim.Name = *req.Name
	}
	if req.Description != nil {
		dim.Description = *req.Description
	}
	if req.GoodDescription != nil {
		dim.GoodDescription = *req.GoodDescription
	}
	if req.BadDescription != nil {
		dim.BadDescription = *req.BadDescription
	}
	if req.IsActive != nil {
		dim.IsActive = *req.IsActive
	}
	if req.Weight != nil {
		dim.Weight = *req.Weight
	}

	// Update using repository
	if err := h.orgRepo.UpdateDimension(c.Request.Context(), dim); err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{
			Error:   "Failed to update dimension",
			Message: err.Error(),
		})
		return
	}

	// Convert to DTO and return
	responseDTO := dto.HealthDimensionDTO{
		ID:              dim.ID,
		Name:            dim.Name,
		Description:     dim.Description,
		GoodDescription: dim.GoodDescription,
		BadDescription:  dim.BadDescription,
		IsActive:        dim.IsActive,
		Weight:          dim.Weight,
		CreatedAt:       dim.CreatedAt,
		UpdatedAt:       dim.UpdatedAt,
	}

	c.JSON(http.StatusOK, responseDTO)
}

// DeleteDimension handles DELETE /api/v1/admin/settings/dimensions/:id
//
// @Summary Delete a health dimension
// @Description Deletes a health dimension. Requires admin (level-1) role.
// @Tags admin-settings
// @Produce json
// @Param id path string true "Dimension ID"
// @Success 200 {object} map[string]string "message"
// @Failure 500 {object} dto.ErrorResponse "Failed to delete dimension"
// @Security BearerAuth
// @Router /admin/settings/dimensions/{id} [delete]
func (h *SettingsAdminHandler) DeleteDimension(c *gin.Context) {
	id := c.Param("id")

	// Delete using repository
	if err := h.orgRepo.DeleteDimension(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{
			Error:   "Failed to delete dimension",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Dimension deleted successfully"})
}

// GetBrandingSettings handles GET /api/v1/admin/settings/branding
//
// @Summary Get branding settings
// @Description Returns the company name and logo URL used for branding. Requires admin (level-1) role.
// @Tags admin-settings
// @Produce json
// @Success 200 {object} dto.BrandingSettings
// @Failure 500 {object} dto.ErrorResponse "Failed to fetch branding settings"
// @Security BearerAuth
// @Router /admin/settings/branding [get]
func (h *SettingsAdminHandler) GetBrandingSettings(c *gin.Context) {
	appSettings, err := h.orgRepo.GetAppSettings(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{Error: "Failed to fetch branding settings", Message: err.Error()})
		return
	}

	settings := dto.BrandingSettings{
		CompanyName: appSettings.CompanyName,
		LogoURL:     appSettings.LogoURL,
	}

	c.JSON(http.StatusOK, settings)
}

// UpdateBrandingSettings handles PUT /api/v1/admin/settings/branding
//
// @Summary Update branding settings
// @Description Updates the company name and/or logo URL (base64 data URL capped at ~500KB). Requires admin (level-1) role.
// @Tags admin-settings
// @Accept json
// @Produce json
// @Param body body dto.BrandingSettings true "Branding settings"
// @Success 200 {object} dto.BrandingSettings
// @Failure 400 {object} dto.ErrorResponse "Invalid request body or logo too large"
// @Failure 500 {object} dto.ErrorResponse "Failed to save branding settings"
// @Security BearerAuth
// @Router /admin/settings/branding [put]
func (h *SettingsAdminHandler) UpdateBrandingSettings(c *gin.Context) {
	var settings dto.BrandingSettings
	if err := c.ShouldBindJSON(&settings); err != nil {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Invalid request body", Message: err.Error()})
		return
	}

	// Validate logo size (base64 data URLs can be large)
	if len(settings.LogoURL) > 700000 {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Logo is too large. Maximum size is 500KB."})
		return
	}

	if err := h.orgRepo.UpdateBrandingSettings(c.Request.Context(), settings.CompanyName, settings.LogoURL); err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{Error: "Failed to save branding settings", Message: err.Error()})
		return
	}

	c.JSON(http.StatusOK, settings)
}

// GetNotificationSettings handles GET /api/v1/admin/settings/notifications
//
// @Summary Get notification settings
// @Description Returns notification configuration (email/Slack enabled, submission reminders, SMTP status). Requires admin (level-1) role.
// @Tags admin-settings
// @Produce json
// @Success 200 {object} dto.NotificationSettings
// @Failure 500 {object} dto.ErrorResponse "Failed to fetch settings"
// @Security BearerAuth
// @Router /admin/settings/notifications [get]
func (h *SettingsAdminHandler) GetNotificationSettings(c *gin.Context) {
	appSettings, err := h.orgRepo.GetAppSettings(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{Error: "Failed to fetch settings", Message: err.Error()})
		return
	}

	settings := dto.NotificationSettings{
		EmailEnabled:       appSettings.EmailNotifications,
		SlackEnabled:       appSettings.SlackNotifications,
		NotifyOnSubmission: appSettings.WeeklyDigest,
		NotifyManagers:     false,
		ReminderDaysBefore: 7,
		ReminderRecipients: []string{},
		SmtpConfigured:     os.Getenv("SMTP_HOST") != "",
	}

	c.JSON(http.StatusOK, settings)
}

// UpdateNotificationSettings handles PUT /api/v1/admin/settings/notifications
//
// @Summary Update notification settings
// @Description Updates notification configuration (email/Slack enabled, weekly digest). Requires admin (level-1) role.
// @Tags admin-settings
// @Accept json
// @Produce json
// @Param body body dto.NotificationSettings true "Notification settings"
// @Success 200 {object} dto.NotificationSettings
// @Failure 400 {object} dto.ErrorResponse "Invalid request body"
// @Failure 500 {object} dto.ErrorResponse "Failed to save settings"
// @Security BearerAuth
// @Router /admin/settings/notifications [put]
func (h *SettingsAdminHandler) UpdateNotificationSettings(c *gin.Context) {
	var settings dto.NotificationSettings
	if err := c.ShouldBindJSON(&settings); err != nil {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Invalid request body", Message: err.Error()})
		return
	}

	if err := h.orgRepo.UpdateNotificationSettings(c.Request.Context(), settings.EmailEnabled, settings.SlackEnabled, settings.NotifyOnSubmission); err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{Error: "Failed to save settings", Message: err.Error()})
		return
	}

	c.JSON(http.StatusOK, settings)
}

// GetRetentionPolicy handles GET /api/v1/admin/settings/retention
//
// @Summary Get data retention policy
// @Description Returns the data retention policy (months to keep sessions, anonymization window). Requires admin (level-1) role.
// @Tags admin-settings
// @Produce json
// @Success 200 {object} dto.RetentionPolicy
// @Failure 500 {object} dto.ErrorResponse "Failed to fetch retention policy"
// @Security BearerAuth
// @Router /admin/settings/retention [get]
func (h *SettingsAdminHandler) GetRetentionPolicy(c *gin.Context) {
	appSettings, err := h.orgRepo.GetAppSettings(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{Error: "Failed to fetch retention policy", Message: err.Error()})
		return
	}

	policy := dto.RetentionPolicy{
		KeepSessionsMonths: appSettings.RetentionMonths,
		ArchiveEnabled:     false,
		AnonymizeAfterDays: appSettings.RetentionMonths * 30,
	}

	c.JSON(http.StatusOK, policy)
}

// UpdateRetentionPolicy handles PUT /api/v1/admin/settings/retention
//
// @Summary Update data retention policy
// @Description Updates the data retention policy. keepSessionsMonths must be between 1 and 120. Requires admin (level-1) role.
// @Tags admin-settings
// @Accept json
// @Produce json
// @Param body body dto.RetentionPolicy true "Retention policy"
// @Success 200 {object} dto.RetentionPolicy
// @Failure 400 {object} dto.ErrorResponse "Invalid request body or keepSessionsMonths out of range"
// @Failure 500 {object} dto.ErrorResponse "Failed to save retention policy"
// @Security BearerAuth
// @Router /admin/settings/retention [put]
func (h *SettingsAdminHandler) UpdateRetentionPolicy(c *gin.Context) {
	var policy dto.RetentionPolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Invalid request body", Message: err.Error()})
		return
	}

	if policy.KeepSessionsMonths < 1 || policy.KeepSessionsMonths > 120 {
		c.JSON(http.StatusBadRequest, dto.ErrorResponse{Error: "Keep sessions months must be between 1 and 120"})
		return
	}

	if err := h.orgRepo.UpdateRetentionSettings(c.Request.Context(), policy.KeepSessionsMonths); err != nil {
		c.JSON(http.StatusInternalServerError, dto.ErrorResponse{Error: "Failed to save retention policy", Message: err.Error()})
		return
	}

	policy.AnonymizeAfterDays = policy.KeepSessionsMonths * 30

	c.JSON(http.StatusOK, policy)
}
