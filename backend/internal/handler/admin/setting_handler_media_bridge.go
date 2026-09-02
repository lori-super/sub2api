package admin

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// GetMediaBridgeSettings returns the inline-media compatibility policy.
// GET /api/v1/admin/settings/media-bridge
func (h *SettingHandler) GetMediaBridgeSettings(c *gin.Context) {
	settings, err := h.settingService.GetMediaBridgeSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	settings.Storage = service.DefaultMediaBridgeSettings().Storage
	if h.mediaBridgeStorage != nil {
		storage, storageErr := h.mediaBridgeStorage.Get(c.Request.Context())
		if storageErr != nil {
			response.ErrorFrom(c, storageErr)
			return
		}
		settings.Storage = storage
	}
	response.Success(c, settings)
}

// UpdateMediaBridgeSettings persists and hot-publishes the inline-media
// compatibility policy. Storage in this document is response-only compatibility
// data: the dedicated step-up storage endpoint is its sole write source.
// PUT /api/v1/admin/settings/media-bridge
func (h *SettingHandler) UpdateMediaBridgeSettings(c *gin.Context) {
	settings, err := h.settingService.GetMediaBridgeSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(settings); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			response.BadRequest(c, "Invalid request: multiple JSON values")
		} else {
			response.BadRequest(c, "Invalid request: "+err.Error())
		}
		return
	}

	// Never persist a public storage snapshot into the policy row. This also
	// removes stale pre-migration values the next time an administrator saves.
	settings.Storage = service.DefaultMediaBridgeSettings().Storage
	if err := service.ValidateMediaBridgeSettings(settings); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if settings.Mode == service.MediaBridgeModeOn || settings.Mode == service.MediaBridgeModeCanary {
		if h.mediaBridgeStorage == nil {
			response.BadRequest(c, "media bridge storage is not configured")
			return
		}
		storage, storageErr := h.mediaBridgeStorage.Get(c.Request.Context())
		if storageErr != nil || !storage.Ready || !storage.SecretConfigured {
			response.BadRequest(c, "media bridge storage must be configured and ready before enabling")
			return
		}
	}

	if err := h.settingService.SetMediaBridgeSettings(c.Request.Context(), settings); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if h.mediaBridgeStorage != nil {
		if storage, storageErr := h.mediaBridgeStorage.Get(c.Request.Context()); storageErr == nil {
			settings.Storage = storage
		}
	}
	response.Success(c, settings)
}

// UpdateMediaBridgeStorage validates, performs an R2 PUT/HEAD/sign/delete
// probe, encrypts the write-only secret and atomically publishes the new store.
// PUT /api/v1/admin/settings/media-bridge/storage
func (h *SettingHandler) UpdateMediaBridgeStorage(c *gin.Context) {
	// Storage credentials remain step-up protected even when the global
	// optional step-up switch is disabled.
	if !middleware.EnforceStepUpAlways(c, h.totpService, h.userService) {
		return
	}
	if h.mediaBridgeStorage == nil {
		response.ErrorFrom(c, service.ErrTemporaryMediaUnavailable)
		return
	}
	var input service.MediaBridgeStorageUpdateInput
	if err := decodeSingleMediaBridgeJSON(c, &input); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	settings, err := h.mediaBridgeStorage.Update(c.Request.Context(), input)
	if err != nil {
		if errors.Is(err, service.ErrSecretEncryptionKeyNotConfigured) {
			response.ErrorFrom(c, err)
			return
		}
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, settings)
}

// TestMediaBridgeStorage exercises the submitted candidate without persisting
// it or changing the store used by new requests.
// POST /api/v1/admin/settings/media-bridge/storage/test
func (h *SettingHandler) TestMediaBridgeStorage(c *gin.Context) {
	if !middleware.EnforceStepUpAlways(c, h.totpService, h.userService) {
		return
	}
	if h.mediaBridgeStorage == nil {
		response.ErrorFrom(c, service.ErrTemporaryMediaUnavailable)
		return
	}
	var input service.MediaBridgeStorageUpdateInput
	if err := decodeSingleMediaBridgeJSON(c, &input); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	_, err := h.mediaBridgeStorage.Test(c.Request.Context(), input)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}

func decodeSingleMediaBridgeJSON(c *gin.Context, target any) error {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
