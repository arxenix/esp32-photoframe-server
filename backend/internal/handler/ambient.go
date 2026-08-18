package handler

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/aitjcize/esp32-photoframe-server/backend/internal/service"
)

// AmbientHandler serves the per-frame Google Photos Ambient pairing endpoints.
// Sync / sync-status / clear / count are served by the generic PhotoSyncHandler.
type AmbientHandler struct {
	ambient *service.AmbientService
}

func NewAmbientHandler(s *service.AmbientService) *AmbientHandler {
	return &AmbientHandler{ambient: s}
}

func frameID(c echo.Context) (uint, error) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	return uint(id), err
}

// Status reports the frame's ambient connection state.
// GET /api/devices/:id/ambient
func (h *AmbientHandler) Status(c echo.Context) error {
	id, err := frameID(c)
	if err != nil {
		return respondError(c, http.StatusBadRequest, "invalid device id")
	}
	status, err := h.ambient.Status(id)
	if err != nil {
		return respondError(c, http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, status)
}

// Connect starts the device-code flow and returns the code/URL to display.
// POST /api/devices/:id/ambient/connect
func (h *AmbientHandler) Connect(c echo.Context) error {
	id, err := frameID(c)
	if err != nil {
		return respondError(c, http.StatusBadRequest, "invalid device id")
	}
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if err := c.Bind(&req); err != nil {
		return respondError(c, http.StatusBadRequest, "invalid request")
	}
	pairing, err := h.ambient.Connect(id, req.DisplayName)
	if err != nil {
		return respondError(c, http.StatusBadRequest, err.Error())
	}
	return c.JSON(http.StatusOK, pairing)
}

// Rename changes the device name shown in the Google Photos app.
// PUT /api/devices/:id/ambient
func (h *AmbientHandler) Rename(c echo.Context) error {
	id, err := frameID(c)
	if err != nil {
		return respondError(c, http.StatusBadRequest, "invalid device id")
	}
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if err := c.Bind(&req); err != nil {
		return respondError(c, http.StatusBadRequest, "invalid request")
	}
	if err := h.ambient.Rename(id, req.DisplayName); err != nil {
		return respondError(c, http.StatusBadRequest, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// Disconnect deletes the frame's ambient device and its local photos.
// DELETE /api/devices/:id/ambient
func (h *AmbientHandler) Disconnect(c echo.Context) error {
	id, err := frameID(c)
	if err != nil {
		return respondError(c, http.StatusBadRequest, "invalid device id")
	}
	if err := h.ambient.Disconnect(id); err != nil {
		return respondError(c, http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "disconnected"})
}
