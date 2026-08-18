package service

import (
	"image"

	"gorm.io/gorm"

	"github.com/aitjcize/esp32-photoframe-server/backend/internal/imagesource"
	"github.com/aitjcize/esp32-photoframe-server/backend/internal/model"
)

// ambientSource is the registry plugin for Google Photos Ambient. The ambient
// service downloads photos to local disk during sync, so serving is the same
// disk read as the gallery source, scoped to the albums bound to this frame
// (each frame has its own ambient device and photo selection).
type ambientSource struct {
	db      *gorm.DB
	dataDir string
}

// NewAmbientSource constructs the plugin.
func NewAmbientSource(db *gorm.DB, dataDir string) imagesource.Source {
	return &ambientSource{db: db, dataDir: dataDir}
}

func (s *ambientSource) Name() string { return model.SourceGoogleAmbient }

func (s *ambientSource) Fetch(req *imagesource.Request) (*imagesource.Response, error) {
	var albumIDs []uint
	if req.Device != nil {
		albumIDs = DeviceAlbumIDs(s.db, req.Device.ID, model.SourceGoogleAmbient)
	}
	pick := func(orientation string, exclude []uint) (model.Image, error) {
		return PickRandomDBPhotoForAlbums(s.db, model.SourceGoogleAmbient, orientation, albumIDs, exclude)
	}
	load := func(item model.Image) (image.Image, error) {
		return LoadLocalPhoto(s.dataDir, item)
	}
	return RunDBPhotoFlow(req, s.db, pick, load)
}
