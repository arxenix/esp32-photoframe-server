package service

// Google Photos Ambient API sync. Unlike the picker integration (a one-shot
// import the user drives from the web UI), an ambient device is registered in
// the user's Google Photos account per frame; the user picks which albums feed
// that frame from inside the Google Photos app, and the server periodically
// re-lists them. Each of the device's media sources becomes an album row bound
// to the frame, so the existing album-sync engine, gallery and device pickers
// work unchanged.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"github.com/aitjcize/esp32-photoframe-server/backend/internal/model"
	"github.com/aitjcize/esp32-photoframe-server/backend/pkg/imageops"
	"github.com/aitjcize/esp32-photoframe-server/backend/pkg/photosambient"
)

// Download size hint for ambient base URLs. Matches the picker import: large
// enough for any e-paper panel, small enough to keep the local cache modest.
const ambientDownloadSize = 1600

// maxCuratedPages bounds how many pages of the curated ambient feed one sync
// pulls. The curated listing may repeat items across pages, so paging it deeply
// mostly burns quota (240 mediaItems.list calls per device per day).
const maxCuratedPages = 3

// Pairing statuses reported to the UI while a frame is being connected.
const (
	AmbientStatusPendingAuth  = "pending_authorization"
	AmbientStatusCreating     = "creating_device"
	AmbientStatusWaitingPhoto = "waiting_for_photos"
	AmbientStatusConnected    = "connected"
	AmbientStatusError        = "error"
)

// AmbientPairing is the in-flight device-code authorization for one frame.
type AmbientPairing struct {
	Status          string    `json:"status"`
	UserCode        string    `json:"user_code,omitempty"`
	VerificationURL string    `json:"verification_url,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type AmbientService struct {
	db       *gorm.DB
	settings *SettingsService
	dataDir  string
	autoSync *AutoSyncScheduler

	mu       sync.Mutex
	pairings map[uint]*AmbientPairing // by local frame id
	lastPoll map[uint]time.Time       // by local frame id
}

func NewAmbientService(db *gorm.DB, settings *SettingsService, dataDir string) *AmbientService {
	svc := &AmbientService{
		db:       db,
		settings: settings,
		dataDir:  dataDir,
		pairings: map[uint]*AmbientPairing{},
		lastPoll: map[uint]time.Time{},
	}
	svc.autoSync = NewAutoSyncScheduler(AutoSyncSchedulerOptions{
		Name:     "Google Ambient",
		Settings: settings,
		IsRelevantKey: func(key string) bool {
			switch key {
			case "google_ambient_auto_sync_enabled", "google_ambient_auto_sync_interval_minutes",
				"google_ambient_client_id", "google_ambient_client_secret":
				return true
			default:
				return false
			}
		},
		IsConfigured: svc.isConfigured,
		GetConfig:    svc.getAutoSyncConfig,
		RunSync:      svc.syncAll,
	})
	return svc
}

// StartAutoSync starts the periodic media sync plus the device-configuration
// poller that watches for the user finishing their photo selection.
func (s *AmbientService) StartAutoSync() {
	s.autoSync.Start()
	go s.pollDevicesLoop()
}

func (s *AmbientService) isConfigured() bool {
	if _, err := s.settings.GetAmbientConfig(); err != nil {
		return false
	}
	var count int64
	s.db.Model(&model.AmbientDevice{}).Where("google_device_id != ''").Count(&count)
	return count > 0
}

func (s *AmbientService) getAutoSyncConfig() (bool, time.Duration) {
	return parseAutoSyncConfig(s.settings,
		"google_ambient_auto_sync_enabled", "google_ambient_auto_sync_interval_minutes")
}

// --- pairing (OAuth device-code flow) ------------------------------------

// Connect starts the device-code flow for one frame and returns the code to
// show the user. Authorization completes in the background; callers poll
// Status. Any previously connected ambient device for the frame is removed
// first so a re-pair doesn't leave an orphan in the user's account.
func (s *AmbientService) Connect(frameID uint, displayName string) (*AmbientPairing, error) {
	cfg, err := s.settings.GetAmbientConfig()
	if err != nil {
		return nil, err
	}
	var frame model.Device
	if err := s.db.First(&frame, frameID).Error; err != nil {
		return nil, err
	}
	if displayName == "" {
		displayName = frame.Name
	}
	if displayName == "" {
		displayName = fmt.Sprintf("PhotoFrame %d", frameID)
	}

	if existing, err := s.deviceFor(frameID); err == nil && existing.Connected() {
		if err := s.Disconnect(frameID); err != nil {
			log.Printf("[ambient] frame %d: removing previous device failed: %v", frameID, err)
		}
	}

	requestID, err := photosambient.NewRequestID()
	if err != nil {
		return nil, err
	}
	auth, err := photosambient.RequestDeviceAuth(context.Background(), cfg, requestID, displayName)
	if err != nil {
		return nil, err
	}

	row := model.AmbientDevice{
		DeviceID:    frameID,
		RequestID:   requestID,
		DisplayName: displayName,
	}
	if err := s.db.Where("device_id = ?", frameID).Assign(map[string]interface{}{
		"request_id":        requestID,
		"display_name":      displayName,
		"google_device_id":  "",
		"settings_uri":      "",
		"media_sources_set": false,
		"access_token":      "",
		"refresh_token":     "",
		"account_email":     "",
		"last_error":        "",
		"updated_at":        time.Now(),
	}).FirstOrCreate(&row).Error; err != nil {
		return nil, err
	}

	pairing := &AmbientPairing{
		Status:          AmbientStatusPendingAuth,
		UserCode:        auth.UserCode,
		VerificationURL: auth.VerificationURI,
		ExpiresAt:       auth.Expiry,
	}
	s.setPairing(frameID, pairing)
	go s.completePairing(frameID, cfg, requestID, displayName, auth)

	copied := *pairing
	return &copied, nil
}

// completePairing waits for the user to authorize, then registers the ambient
// device with the same requestId used for the code request (which is what makes
// Google redirect the user straight to the device's photo-selection screen).
func (s *AmbientService) completePairing(
	frameID uint, cfg photosambient.Config, requestID, displayName string,
	auth *oauth2.DeviceAuthResponse,
) {
	fail := func(err error) {
		log.Printf("[ambient] frame %d: pairing failed: %v", frameID, err)
		s.setPairing(frameID, &AmbientPairing{Status: AmbientStatusError, Error: err.Error()})
		s.db.Model(&model.AmbientDevice{}).Where("device_id = ?", frameID).
			Updates(map[string]interface{}{"last_error": err.Error(), "updated_at": time.Now()})
	}

	ctx, cancel := context.WithDeadline(context.Background(), auth.Expiry)
	defer cancel()

	token, err := photosambient.WaitForToken(ctx, cfg, auth)
	if err != nil {
		fail(err)
		return
	}
	if err := s.saveToken(frameID, token); err != nil {
		fail(err)
		return
	}
	s.setPairing(frameID, &AmbientPairing{Status: AmbientStatusCreating})

	row, err := s.deviceFor(frameID)
	if err != nil {
		fail(err)
		return
	}
	client, err := s.clientFor(row)
	if err != nil {
		fail(err)
		return
	}

	dev, err := client.CreateDevice(ctx, requestID, displayName)
	if photosambient.IsStatus(err, "ALREADY_EXISTS") {
		// An earlier attempt created the device but we never stored its id;
		// the requestId doubles as a handle for deleting that orphan.
		if delErr := client.DeleteDevice(ctx, requestID); delErr != nil {
			fail(fmt.Errorf("device already exists and could not be removed: %w", delErr))
			return
		}
		dev, err = client.CreateDevice(ctx, requestID, displayName)
	}
	if err != nil {
		fail(err)
		return
	}

	email, err := client.AccountEmail(ctx)
	if err != nil {
		log.Printf("[ambient] frame %d: account lookup failed: %v", frameID, err)
	}
	if err := s.db.Model(&model.AmbientDevice{}).Where("id = ?", row.ID).
		Updates(map[string]interface{}{
			"google_device_id": dev.ID,
			"account_email":    email,
			"last_error":       "",
			"updated_at":       time.Now(),
		}).Error; err != nil {
		fail(err)
		return
	}
	s.applyDeviceState(row.ID, dev)

	if dev.MediaSourcesSet {
		s.setPairing(frameID, &AmbientPairing{Status: AmbientStatusConnected})
		s.autoSync.SyncNowAsync()
		return
	}
	s.setPairing(frameID, &AmbientPairing{Status: AmbientStatusWaitingPhoto})
}

// Disconnect removes the frame's ambient device from the user's Google Photos
// account and drops its local albums, photos and authorization.
func (s *AmbientService) Disconnect(frameID uint) error {
	row, err := s.deviceFor(frameID)
	if err != nil {
		return err
	}
	if row.RefreshToken != "" {
		if client, cErr := s.clientFor(row); cErr == nil {
			target := row.GoogleDeviceID
			if target == "" {
				target = row.RequestID // orphaned device: delete by requestId
			}
			if target != "" {
				if err := client.DeleteDevice(context.Background(), target); err != nil &&
					!photosambient.IsStatus(err, "NOT_FOUND") {
					log.Printf("[ambient] frame %d: devices.delete failed: %v", frameID, err)
				}
			}
		}
	}

	if row.GoogleDeviceID != "" {
		// Album rows cascade to memberships and device mappings.
		if err := s.db.Where("source = ? AND external_id LIKE ?",
			model.SourceGoogleAmbient, row.GoogleDeviceID+":%").
			Delete(&model.Album{}).Error; err != nil {
			return err
		}
	}
	if err := s.db.Delete(&model.AmbientDevice{}, row.ID).Error; err != nil {
		return err
	}
	s.clearPairing(frameID)
	gcOrphanImagesForSource(s.db, model.SourceGoogleAmbient)
	s.gcPhotoFiles()
	return nil
}

// Rename updates the display name of the frame's ambient device (the name the
// user sees in the Google Photos app; only editable through the API).
func (s *AmbientService) Rename(frameID uint, displayName string) error {
	if displayName == "" {
		return errors.New("display name required")
	}
	row, err := s.deviceFor(frameID)
	if err != nil {
		return err
	}
	if row.GoogleDeviceID != "" {
		client, err := s.clientFor(row)
		if err != nil {
			return err
		}
		if _, err := client.RenameDevice(context.Background(), row.GoogleDeviceID, displayName); err != nil {
			return err
		}
	}
	return s.db.Model(&model.AmbientDevice{}).Where("id = ?", row.ID).
		Updates(map[string]interface{}{"display_name": displayName, "updated_at": time.Now()}).Error
}

// AmbientStatus is the per-frame connection state the settings UI renders.
type AmbientStatus struct {
	Configured bool                        `json:"configured"`
	Connected  bool                        `json:"connected"`
	Device     *model.AmbientDevice        `json:"device,omitempty"`
	Sources    []photosambient.MediaSource `json:"media_sources,omitempty"`
	Pairing    *AmbientPairing             `json:"pairing,omitempty"`
	Syncing    bool                        `json:"syncing"`
	PhotoCount int64                       `json:"photo_count"`
}

// Status reports the frame's ambient state, including any in-flight pairing.
func (s *AmbientService) Status(frameID uint) (*AmbientStatus, error) {
	_, cfgErr := s.settings.GetAmbientConfig()
	status := &AmbientStatus{Configured: cfgErr == nil, Syncing: s.autoSync.IsRunning()}
	status.Pairing = s.pairing(frameID)

	row, err := s.deviceFor(frameID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return status, nil
		}
		return nil, err
	}
	status.Device = row
	status.Connected = row.Connected()

	var albums []model.Album
	if err := s.db.Where("source = ? AND external_id LIKE ?",
		model.SourceGoogleAmbient, row.GoogleDeviceID+":%").Find(&albums).Error; err == nil {
		for _, a := range albums {
			if !a.SyncEnabled {
				continue
			}
			_, sourceID, _ := strings.Cut(a.ExternalID, ":")
			status.Sources = append(status.Sources, photosambient.MediaSource{
				ID: sourceID, DisplayName: a.Name,
			})
		}
	}
	if len(albums) > 0 {
		albumIDs := make([]uint, 0, len(albums))
		for _, a := range albums {
			albumIDs = append(albumIDs, a.ID)
		}
		s.db.Model(&model.Image{}).
			Joins("JOIN image_album_memberships m ON m.image_id = images.id").
			Where("images.source = ? AND m.album_id IN ?", model.SourceGoogleAmbient, albumIDs).
			Distinct("images.id").Count(&status.PhotoCount)
	}
	return status, nil
}

func (s *AmbientService) deviceFor(frameID uint) (*model.AmbientDevice, error) {
	var row model.AmbientDevice
	if err := s.db.Where("device_id = ?", frameID).First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *AmbientService) setPairing(frameID uint, p *AmbientPairing) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairings[frameID] = p
}

func (s *AmbientService) clearPairing(frameID uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pairings, frameID)
}

func (s *AmbientService) pairing(frameID uint) *AmbientPairing {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pairings[frameID]; ok {
		copied := *p
		return &copied
	}
	return nil
}

// --- authorization plumbing ---------------------------------------------

func (s *AmbientService) saveToken(frameID uint, token *oauth2.Token) error {
	expiry := token.Expiry
	return s.db.Model(&model.AmbientDevice{}).Where("device_id = ?", frameID).
		Updates(map[string]interface{}{
			"access_token":  token.AccessToken,
			"refresh_token": token.RefreshToken,
			"expiry":        &expiry,
			"updated_at":    time.Now(),
		}).Error
}

// clientFor builds an Ambient API client authorized as the frame's account,
// persisting refreshed tokens back onto the frame's row.
func (s *AmbientService) clientFor(row *model.AmbientDevice) (*photosambient.Client, error) {
	cfg, err := s.settings.GetAmbientConfig()
	if err != nil {
		return nil, err
	}
	if row.RefreshToken == "" && row.AccessToken == "" {
		return nil, errors.New("ambient device is not authorized")
	}
	token := &oauth2.Token{
		AccessToken:  row.AccessToken,
		RefreshToken: row.RefreshToken,
		TokenType:    "Bearer",
	}
	if row.Expiry != nil {
		token.Expiry = *row.Expiry
	}
	ctx := context.Background()
	conf := photosambient.OAuthConfig(cfg)
	source := oauth2.ReuseTokenSource(token, &ambientTokenSaver{
		source: conf.TokenSource(ctx, token),
		db:     s.db,
		rowID:  row.ID,
	})
	return photosambient.NewClient(oauth2.NewClient(ctx, source)), nil
}

// ambientTokenSaver persists tokens refreshed by the oauth2 client. Google omits
// the refresh token on refresh responses, so the stored one is kept.
type ambientTokenSaver struct {
	source oauth2.TokenSource
	db     *gorm.DB
	rowID  uint
}

func (t *ambientTokenSaver) Token() (*oauth2.Token, error) {
	token, err := t.source.Token()
	if err != nil {
		return nil, err
	}
	updates := map[string]interface{}{
		"access_token": token.AccessToken,
		"expiry":       &token.Expiry,
		"updated_at":   time.Now(),
	}
	if token.RefreshToken != "" {
		updates["refresh_token"] = token.RefreshToken
	}
	if err := t.db.Model(&model.AmbientDevice{}).Where("id = ?", t.rowID).
		Updates(updates).Error; err != nil {
		return nil, err
	}
	return token, nil
}

// --- device configuration polling ---------------------------------------

// pollDevicesLoop watches frames whose user hasn't finished picking photos yet,
// at the interval Google recommends per device, and kicks off a sync as soon as
// media sources appear.
func (s *AmbientService) pollDevicesLoop() {
	for {
		time.Sleep(15 * time.Second)

		var rows []model.AmbientDevice
		if err := s.db.Where("google_device_id != '' AND media_sources_set = ?", false).
			Find(&rows).Error; err != nil {
			continue
		}
		for i := range rows {
			row := &rows[i]
			if !s.pollDue(row) {
				continue
			}
			dev, err := s.refreshDevice(row)
			if err != nil {
				log.Printf("[ambient] frame %d: devices.get failed: %v", row.DeviceID, err)
				continue
			}
			if dev.MediaSourcesSet {
				s.setPairing(row.DeviceID, &AmbientPairing{Status: AmbientStatusConnected})
				s.autoSync.SyncNowAsync()
			}
		}
	}
}

func (s *AmbientService) pollDue(row *model.AmbientDevice) bool {
	interval := time.Duration(row.PollIntervalSeconds) * time.Second
	if interval < 15*time.Second {
		interval = 15 * time.Second
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.lastPoll[row.DeviceID]; ok && time.Since(last) < interval {
		return false
	}
	s.lastPoll[row.DeviceID] = time.Now()
	return true
}

// refreshDevice re-reads the device from Google and mirrors its state (photo
// selection, polling interval, display name) plus its media-source albums.
func (s *AmbientService) refreshDevice(row *model.AmbientDevice) (*photosambient.AmbientDevice, error) {
	client, err := s.clientFor(row)
	if err != nil {
		return nil, err
	}
	dev, err := client.GetDevice(context.Background(), row.GoogleDeviceID)
	if err != nil {
		s.db.Model(&model.AmbientDevice{}).Where("id = ?", row.ID).
			Updates(map[string]interface{}{"last_error": err.Error(), "updated_at": time.Now()})
		return nil, err
	}
	s.applyDeviceState(row.ID, dev)
	if err := s.syncAlbums(row, dev); err != nil {
		return nil, err
	}
	row.MediaSourcesSet = dev.MediaSourcesSet
	return dev, nil
}

func (s *AmbientService) applyDeviceState(rowID uint, dev *photosambient.AmbientDevice) {
	updates := map[string]interface{}{
		"settings_uri":      dev.SettingsURI,
		"media_sources_set": dev.MediaSourcesSet,
		"last_error":        "",
		"updated_at":        time.Now(),
	}
	if dev.DisplayName != "" {
		updates["display_name"] = dev.DisplayName
	}
	if interval := dev.PollingConfig.Interval(); interval > 0 {
		updates["poll_interval_seconds"] = int(interval.Seconds())
	}
	if err := s.db.Model(&model.AmbientDevice{}).Where("id = ?", rowID).
		Updates(updates).Error; err != nil {
		log.Printf("[ambient] update device row %d: %v", rowID, err)
	}
}

// syncAlbums mirrors the device's media sources as album rows bound to the
// frame, so a frame draws only from the photos its own user selected. Sources
// the user removed are disabled, which drops their photos on the next GC.
func (s *AmbientService) syncAlbums(row *model.AmbientDevice, dev *photosambient.AmbientDevice) error {
	wanted := make([]string, 0, len(dev.MediaSources))
	for _, src := range dev.MediaSources {
		if src.ID == "" {
			continue
		}
		externalID := ambientAlbumExternalID(row.GoogleDeviceID, src.ID)
		wanted = append(wanted, externalID)

		name := src.DisplayName
		if name == "" {
			name = src.ID
		}
		if row.DisplayName != "" {
			name = fmt.Sprintf("%s (%s)", name, row.DisplayName)
		}

		var album model.Album
		err := s.db.Where("source = ? AND external_id = ?",
			model.SourceGoogleAmbient, externalID).First(&album).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			album = model.Album{
				Source:      model.SourceGoogleAmbient,
				ExternalID:  externalID,
				Name:        name,
				Kind:        model.AlbumKindReal,
				SyncEnabled: true,
				UpdatedAt:   time.Now(),
			}
			if err := s.db.Create(&album).Error; err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if err := s.db.Model(&model.Album{}).Where("id = ?", album.ID).
				Updates(map[string]interface{}{
					"name": name, "sync_enabled": true, "updated_at": time.Now(),
				}).Error; err != nil {
				return err
			}
		}

		// Bind the album to its frame; ambient photo selection is per frame.
		if err := s.db.Where(model.DeviceAlbumMapping{DeviceID: row.DeviceID, AlbumID: album.ID}).
			FirstOrCreate(&model.DeviceAlbumMapping{
				DeviceID: row.DeviceID, AlbumID: album.ID,
			}).Error; err != nil {
			return err
		}
	}

	// Disable this device's albums whose media source the user removed.
	q := s.db.Model(&model.Album{}).Where("source = ? AND external_id LIKE ?",
		model.SourceGoogleAmbient, row.GoogleDeviceID+":%")
	if len(wanted) > 0 {
		q = q.Where("external_id NOT IN ?", wanted)
	}
	return q.Update("sync_enabled", false).Error
}

func ambientAlbumExternalID(googleDeviceID, mediaSourceID string) string {
	return googleDeviceID + ":" + mediaSourceID
}

// --- media sync ---------------------------------------------------------

// Source implements AlbumSource.
func (s *AmbientService) Source() string { return model.SourceGoogleAmbient }

// ListRemoteAlbums implements AlbumSource. Album names are maintained by
// syncAlbums from the device's media sources, so there is nothing to refresh.
func (s *AmbientService) ListRemoteAlbums() ([]RemoteAlbum, error) { return nil, nil }

// FetchAlbumAssets implements AlbumSource: it lists the media items of one
// media source of one frame's ambient device and downloads any it hasn't cached
// yet (ambient base URLs expire after about an hour, so bytes must be fetched
// while the listing is fresh).
func (s *AmbientService) FetchAlbumAssets(album model.Album) ([]RemoteAsset, error) {
	googleDeviceID, mediaSourceID, ok := strings.Cut(album.ExternalID, ":")
	if !ok {
		return nil, fmt.Errorf("malformed ambient album id %q", album.ExternalID)
	}

	var row model.AmbientDevice
	if err := s.db.Where("google_device_id = ?", googleDeviceID).First(&row).Error; err != nil {
		return nil, err
	}
	if !row.MediaSourcesSet {
		return nil, errors.New("user has not selected photos for this device yet")
	}
	client, err := s.clientFor(&row)
	if err != nil {
		return nil, err
	}

	items, err := s.listMediaItems(client, &row, mediaSourceID)
	if err != nil {
		return nil, err
	}
	return s.cacheItems(album, items)
}

// listMediaItems pages one media source. The `highlights` source cannot be
// listed directly, so it is served by the curated (unfiltered) feed instead;
// that feed may repeat items across pages, hence the page cap.
func (s *AmbientService) listMediaItems(
	client *photosambient.Client, row *model.AmbientDevice, mediaSourceID string,
) ([]photosambient.MediaItem, error) {
	curated := mediaSourceID == photosambient.HighlightsSourceID
	opts := photosambient.ListMediaItemsOptions{
		DeviceID: row.GoogleDeviceID,
		PageSize: photosambient.MaxPageSize,
	}
	if !curated {
		opts.MediaSourceID = mediaSourceID
	}

	var (
		items []photosambient.MediaItem
		seen  = map[string]bool{}
	)
	for page := 0; ; page++ {
		if curated && page >= maxCuratedPages {
			break
		}
		if err := s.reserveListCall(row); err != nil {
			// Out of daily quota: keep what we have rather than failing the sync.
			log.Printf("[ambient] frame %d: %v", row.DeviceID, err)
			break
		}
		resp, err := client.ListMediaItems(context.Background(), opts)
		if err != nil {
			if len(items) > 0 {
				log.Printf("[ambient] frame %d: partial listing: %v", row.DeviceID, err)
				break
			}
			return nil, err
		}
		added := 0
		for _, item := range resp.MediaItems {
			if item.ID == "" || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			items = append(items, item)
			added++
		}
		if resp.NextPageToken == "" || added == 0 {
			break
		}
		opts.PageToken = resp.NextPageToken
	}
	return items, nil
}

// ambientToday is the UTC day the ambient request quota is counted against.
func ambientToday() string { return time.Now().UTC().Format("2006-01-02") }

// reserveListCall accounts one mediaItems.list request against the API's cap of
// 240 requests per device per day (counter resets on UTC date change).
func (s *AmbientService) reserveListCall(row *model.AmbientDevice) error {
	today := ambientToday()
	if row.ListCallsDate != today {
		row.ListCallsDate = today
		row.ListCallsCount = 0
	}
	if row.ListCallsCount >= photosambient.DailyListQuota {
		return fmt.Errorf("daily quota of %d mediaItems.list requests reached", photosambient.DailyListQuota)
	}
	row.ListCallsCount++
	return s.db.Model(&model.AmbientDevice{}).Where("id = ?", row.ID).
		Updates(map[string]interface{}{
			"list_calls_date":  row.ListCallsDate,
			"list_calls_count": row.ListCallsCount,
		}).Error
}

// cacheItems downloads the bytes of items not cached yet and returns the
// album's assets. Items already in the DB are re-emitted unchanged so the
// album-sync engine keeps them; videos and items without a base URL are skipped.
func (s *AmbientService) cacheItems(album model.Album, items []photosambient.MediaItem) ([]RemoteAsset, error) {
	existing := map[string]model.Image{}
	if len(items) > 0 {
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		var rows []model.Image
		if err := s.db.Where("source = ? AND external_id IN ?",
			model.SourceGoogleAmbient, ids).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			existing[r.ExternalID] = r
		}
	}

	photosDir := filepath.Join(s.dataDir, "photos", "ambient")
	if err := os.MkdirAll(photosDir, 0755); err != nil {
		return nil, err
	}

	assets := make([]RemoteAsset, 0, len(items))
	for _, item := range items {
		if prior, ok := existing[item.ID]; ok {
			if _, err := os.Stat(ResolveLocalPath(s.dataDir, prior.FilePath)); err == nil {
				assets = append(assets, RemoteAsset{
					ExternalID:   prior.ExternalID,
					FilePath:     prior.FilePath,
					Width:        prior.Width,
					Height:       prior.Height,
					Orientation:  prior.Orientation,
					PhotoTakenAt: prior.PhotoTakenAt,
				})
				continue
			}
			// Cached row lost its file; re-download over the same row.
			s.db.Unscoped().Delete(&model.Image{}, prior.ID)
		}
		if !item.IsImage() || item.MediaFile.BaseURL == "" {
			continue
		}
		asset, err := s.downloadItem(photosDir, item)
		if err != nil {
			log.Printf("[ambient] album %q: download %s failed: %v", album.Name, item.ID, err)
			continue
		}
		assets = append(assets, *asset)
	}
	return assets, nil
}

func (s *AmbientService) downloadItem(photosDir string, item photosambient.MediaItem) (*RemoteAsset, error) {
	// Media item ids are long and opaque; hash them into a stable filename.
	sum := sha256.Sum256([]byte(item.ID))
	localPath := filepath.Join(photosDir, hex.EncodeToString(sum[:16])+".jpg")

	resp, err := http.Get(item.DownloadURL(ambientDownloadSize, ambientDownloadSize))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: status %d", resp.StatusCode)
	}

	out, err := os.Create(localPath)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(out, resp.Body)
	out.Close()
	if err != nil {
		os.Remove(localPath)
		return nil, err
	}

	// Google's CDN usually returns pre-rotated pixels for sized URLs, but that
	// isn't guaranteed; baking the EXIF orientation in is a cheap safety net.
	if err := imageops.AutoOrient(localPath); err != nil {
		log.Printf("[ambient] auto-orient %s: %v", localPath, err)
	}

	width, height := item.MediaFile.MediaFileMetadata.Width, item.MediaFile.MediaFileMetadata.Height
	if f, err := os.Open(localPath); err == nil {
		cfg, _, decErr := image.DecodeConfig(f)
		f.Close()
		if decErr == nil {
			width, height = cfg.Width, cfg.Height
		}
	}

	return &RemoteAsset{
		ExternalID:   item.ID,
		FilePath:     localPath,
		Width:        width,
		Height:       height,
		Orientation:  determineOrientation(width, height, ""),
		PhotoTakenAt: item.Created(),
	}, nil
}

// syncAll refreshes every connected frame's device state, then imports its
// selected photos through the shared album-sync engine.
func (s *AmbientService) syncAll() error {
	var rows []model.AmbientDevice
	if err := s.db.Where("google_device_id != ''").Find(&rows).Error; err != nil {
		return err
	}
	for i := range rows {
		if _, err := s.refreshDevice(&rows[i]); err != nil {
			log.Printf("[ambient] frame %d: refresh failed: %v", rows[i].DeviceID, err)
		}
	}

	_, err := SyncAlbumSource(s.db, s)
	s.gcPhotoFiles()

	now := time.Now()
	updates := map[string]interface{}{"last_sync_at": &now, "updated_at": now}
	if err != nil {
		updates["last_error"] = err.Error()
	}
	s.db.Model(&model.AmbientDevice{}).Where("google_device_id != ''").Updates(updates)
	return err
}

// gcPhotoFiles removes cached ambient files whose image row is gone (the shared
// engine prunes rows in bulk SQL and can't clean up files itself).
func (s *AmbientService) gcPhotoFiles() {
	photosDir := filepath.Join(s.dataDir, "photos", "ambient")
	entries, err := os.ReadDir(photosDir)
	if err != nil {
		return
	}
	var paths []string
	if err := s.db.Model(&model.Image{}).Where("source = ?", model.SourceGoogleAmbient).
		Pluck("file_path", &paths).Error; err != nil {
		return // on DB error keep the files: orphans beat missing photos
	}
	alive := make(map[string]bool, len(paths))
	for _, p := range paths {
		alive[filepath.Base(p)] = true
	}
	for _, entry := range entries {
		if entry.IsDir() || alive[entry.Name()] {
			continue
		}
		os.Remove(filepath.Join(photosDir, entry.Name()))
	}
}

// --- PhotoSyncBackend ---------------------------------------------------

// ClearAndResync triggers a background incremental sync (no clear; see
// ImmichService.resyncInternal for why).
func (s *AmbientService) ClearAndResync() error {
	s.autoSync.SyncNowAsync()
	return nil
}

// IsSyncing reports whether an ambient sync is in flight.
func (s *AmbientService) IsSyncing() bool { return s.autoSync.IsRunning() }

// LastSyncError reports the most recent sync run's failure ("" on success).
func (s *AmbientService) LastSyncError() string { return s.autoSync.LastError() }

// ClearPhotos deletes all locally cached ambient photos.
func (s *AmbientService) ClearPhotos() error {
	if err := clearSourcePhotos(s.db, model.SourceGoogleAmbient); err != nil {
		return err
	}
	s.gcPhotoFiles()
	return nil
}

// GetPhotoCount returns the number of locally cached ambient photos.
func (s *AmbientService) GetPhotoCount() (int64, error) {
	return sourcePhotoCount(s.db, model.SourceGoogleAmbient)
}
