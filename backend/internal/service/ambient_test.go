package service

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/aitjcize/esp32-photoframe-server/backend/internal/model"
	"github.com/aitjcize/esp32-photoframe-server/backend/pkg/photosambient"
)

func setupAmbientTestService(t *testing.T) (*AmbientService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Setting{}, &model.Device{}, &model.Album{}, &model.Image{},
		&model.ImageAlbumMembership{}, &model.DeviceAlbumMapping{}, &model.AmbientDevice{},
	))
	return NewAmbientService(db, NewSettingsService(db), t.TempDir()), db
}

func TestSyncAlbumsMirrorsMediaSources(t *testing.T) {
	svc, db := setupAmbientTestService(t)
	frame := model.Device{Name: "Kitchen"}
	require.NoError(t, db.Create(&frame).Error)
	row := model.AmbientDevice{DeviceID: frame.ID, GoogleDeviceID: "gdev", DisplayName: "Kitchen"}
	require.NoError(t, db.Create(&row).Error)

	dev := &photosambient.AmbientDevice{
		MediaSourcesSet: true,
		MediaSources: []photosambient.MediaSource{
			{ID: "src1", DisplayName: "Vacation"},
			{ID: "highlights"},
		},
	}
	require.NoError(t, svc.syncAlbums(&row, dev))

	var albums []model.Album
	require.NoError(t, db.Where("source = ?", model.SourceGoogleAmbient).
		Order("external_id").Find(&albums).Error)
	require.Len(t, albums, 2)
	assert.Equal(t, "gdev:highlights", albums[0].ExternalID)
	assert.Equal(t, "highlights (Kitchen)", albums[0].Name)
	assert.Equal(t, "Vacation (Kitchen)", albums[1].Name)
	for _, a := range albums {
		assert.True(t, a.SyncEnabled)
		// Each album is bound to the frame it was configured for.
		var mappings int64
		db.Model(&model.DeviceAlbumMapping{}).
			Where("device_id = ? AND album_id = ?", frame.ID, a.ID).Count(&mappings)
		assert.Equal(t, int64(1), mappings, "album %q not bound to its frame", a.Name)
	}

	// The user removes a source in the Google Photos app: its album is disabled
	// (which lets the shared GC drop its photos) but kept for its memberships.
	dev.MediaSources = dev.MediaSources[:1]
	require.NoError(t, svc.syncAlbums(&row, dev))
	require.NoError(t, db.Where("source = ?", model.SourceGoogleAmbient).
		Order("external_id").Find(&albums).Error)
	require.Len(t, albums, 2)
	assert.False(t, albums[0].SyncEnabled, "removed source stays disabled")
	assert.True(t, albums[1].SyncEnabled)

	// Re-running is idempotent.
	require.NoError(t, svc.syncAlbums(&row, dev))
	var count int64
	db.Model(&model.Album{}).Where("source = ?", model.SourceGoogleAmbient).Count(&count)
	assert.Equal(t, int64(2), count)
}

func TestReserveListCallEnforcesDailyQuota(t *testing.T) {
	svc, db := setupAmbientTestService(t)
	row := model.AmbientDevice{DeviceID: 1, GoogleDeviceID: "gdev"}
	require.NoError(t, db.Create(&row).Error)

	for i := 0; i < photosambient.DailyListQuota; i++ {
		require.NoError(t, svc.reserveListCall(&row))
	}
	err := svc.reserveListCall(&row)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daily quota")

	var stored model.AmbientDevice
	require.NoError(t, db.First(&stored, row.ID).Error)
	assert.Equal(t, photosambient.DailyListQuota, stored.ListCallsCount)

	// A new UTC day resets the counter.
	row.ListCallsDate = "1999-01-01"
	require.NoError(t, svc.reserveListCall(&row))
	assert.Equal(t, 1, row.ListCallsCount)
}

// ambientListServer serves paginated mediaItems.list responses.
func ambientListServer(t *testing.T, pages [][]string) (*photosambient.Client, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 0
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			_, _ = fmt.Sscanf(tok, "page-%d", &page)
		}
		calls++
		body := `{"mediaItems":[`
		for i, id := range pages[page] {
			if i > 0 {
				body += ","
			}
			body += fmt.Sprintf(
				`{"id":%q,"mediaFile":{"baseUrl":"%s/img/%s","mimeType":"image/jpeg","mediaFileMetadata":{"width":100,"height":50}}}`,
				id, "http://"+r.Host, id)
		}
		body += "]"
		if page+1 < len(pages) {
			body += fmt.Sprintf(`,"nextPageToken":"page-%d"`, page+1)
		}
		body += "}"
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return photosambient.NewClient(srv.Client()).WithBaseURL(srv.URL), &calls
}

func TestListMediaItemsPagesAndDedupes(t *testing.T) {
	svc, db := setupAmbientTestService(t)
	row := model.AmbientDevice{DeviceID: 1, GoogleDeviceID: "gdev"}
	require.NoError(t, db.Create(&row).Error)

	client, calls := ambientListServer(t, [][]string{
		{"a", "b"}, {"b", "c"}, {"d"},
	})
	items, err := svc.listMediaItems(client, &row, "src1")
	require.NoError(t, err)

	var ids []string
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	assert.Equal(t, []string{"a", "b", "c", "d"}, ids)
	assert.Equal(t, 3, *calls)
	assert.Equal(t, 3, row.ListCallsCount, "every request counts against the quota")
}

func TestListMediaItemsCapsCuratedFeed(t *testing.T) {
	svc, db := setupAmbientTestService(t)
	row := model.AmbientDevice{DeviceID: 1, GoogleDeviceID: "gdev"}
	require.NoError(t, db.Create(&row).Error)

	// The curated feed is effectively endless, so paging it must be bounded.
	pages := make([][]string, 20)
	for i := range pages {
		pages[i] = []string{fmt.Sprintf("item-%d", i)}
	}
	client, calls := ambientListServer(t, pages)
	items, err := svc.listMediaItems(client, &row, photosambient.HighlightsSourceID)
	require.NoError(t, err)
	assert.Len(t, items, maxCuratedPages)
	assert.Equal(t, maxCuratedPages, *calls)
}

func TestListMediaItemsStopsWhenQuotaExhausted(t *testing.T) {
	svc, db := setupAmbientTestService(t)
	row := model.AmbientDevice{
		DeviceID: 1, GoogleDeviceID: "gdev",
		ListCallsCount: photosambient.DailyListQuota,
	}
	row.ListCallsDate = ambientToday()
	require.NoError(t, db.Create(&row).Error)

	client, calls := ambientListServer(t, [][]string{{"a"}})
	items, err := svc.listMediaItems(client, &row, "src1")
	require.NoError(t, err, "running out of quota keeps the existing photos")
	assert.Empty(t, items)
	assert.Zero(t, *calls)
}

func TestCacheItemsDownloadsSkippingVideosAndReusingCache(t *testing.T) {
	svc, db := setupAmbientTestService(t)
	album := model.Album{Source: model.SourceGoogleAmbient, ExternalID: "gdev:src1", SyncEnabled: true}
	require.NoError(t, db.Create(&album).Error)

	// 1x1 white JPEG served for every download.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.RawQuery+r.URL.Path, "w1600-h1600", "download must bound the size")
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(testJPEG(t))
	}))
	defer srv.Close()

	items := []photosambient.MediaItem{
		{ID: "photo", MediaFile: photosambient.MediaFile{BaseURL: srv.URL + "/p", MimeType: "image/jpeg"}},
		{ID: "movie", MediaFile: photosambient.MediaFile{BaseURL: srv.URL + "/v", MimeType: "video/mp4"}},
		{ID: "broken", MediaFile: photosambient.MediaFile{MimeType: "image/jpeg"}},
	}
	assets, err := svc.cacheItems(album, items)
	require.NoError(t, err)
	require.Len(t, assets, 1, "videos and items without a base url are skipped")
	assert.Equal(t, "photo", assets[0].ExternalID)
	assert.FileExists(t, assets[0].FilePath)
	assert.Equal(t, 1, assets[0].Width)

	// Persist the row as the sync engine would, then re-sync: the cached file is
	// reused rather than downloaded again.
	_, _, err = upsertAlbumAssets(db, model.SourceGoogleAmbient, album.ID, assets)
	require.NoError(t, err)

	srv.Close() // any further download attempt now fails
	again, err := svc.cacheItems(album, items)
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.Equal(t, assets[0].FilePath, again[0].FilePath)

	// A cached row whose file vanished is re-fetched (and fails loudly-but-safely
	// here, since the server is gone).
	require.NoError(t, os.Remove(assets[0].FilePath))
	empty, err := svc.cacheItems(album, items)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// testJPEG returns a minimal 1x1 JPEG.
func testJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1)), nil))
	return buf.Bytes()
}

func TestGCPhotoFilesRemovesUnreferencedFiles(t *testing.T) {
	svc, db := setupAmbientTestService(t)
	dir := filepath.Join(svc.dataDir, "photos", "ambient")
	require.NoError(t, os.MkdirAll(dir, 0755))
	kept := filepath.Join(dir, "kept.jpg")
	orphan := filepath.Join(dir, "orphan.jpg")
	require.NoError(t, os.WriteFile(kept, []byte("x"), 0644))
	require.NoError(t, os.WriteFile(orphan, []byte("x"), 0644))
	require.NoError(t, db.Create(&model.Image{
		Source: model.SourceGoogleAmbient, ExternalID: "kept", FilePath: kept,
	}).Error)

	svc.gcPhotoFiles()
	assert.FileExists(t, kept)
	assert.NoFileExists(t, orphan)
}
