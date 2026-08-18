package service

import (
	"errors"
	"sync"

	"github.com/aitjcize/esp32-photoframe-server/backend/internal/model"
	"github.com/aitjcize/esp32-photoframe-server/backend/pkg/googlephotos"
	"github.com/aitjcize/esp32-photoframe-server/backend/pkg/photosambient"
	"gorm.io/gorm"
)

// CalendarConfigProvider wraps SettingsService to provide calendar-specific OAuth config.
// It implements googlephotos.ConfigProvider so we can create a separate OAuth client for calendar.
type CalendarConfigProvider struct {
	settings *SettingsService
}

func NewCalendarConfigProvider(s *SettingsService) *CalendarConfigProvider {
	return &CalendarConfigProvider{settings: s}
}

func (p *CalendarConfigProvider) GetGoogleConfig() (googlephotos.Config, error) {
	return p.settings.GetGoogleCalendarConfig()
}

type SettingsService struct {
	db        *gorm.DB
	mu        sync.Mutex
	callbacks []func(key, value string)
}

func NewSettingsService(db *gorm.DB) *SettingsService {
	return &SettingsService{db: db}
}

func (s *SettingsService) Get(key string) (string, error) {
	var setting model.Setting
	result := s.db.First(&setting, "key = ?", key)
	if result.Error != nil {
		return "", result.Error
	}
	return setting.Value, nil
}

func (s *SettingsService) Set(key string, value string) error {
	setting := model.Setting{Key: key, Value: value}
	// Save will create or update
	if err := s.db.Save(&setting).Error; err != nil {
		return err
	}
	s.notifyChanged(key, value)
	return nil
}

func (s *SettingsService) RegisterOnChange(callback func(key, value string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callbacks = append(s.callbacks, callback)
}

func (s *SettingsService) notifyChanged(key, value string) {
	s.mu.Lock()
	callbacks := append([]func(key, value string){}, s.callbacks...)
	s.mu.Unlock()

	for _, callback := range callbacks {
		callback(key, value)
	}
}

func (s *SettingsService) GetAll() (map[string]string, error) {
	var settings []model.Setting
	result := s.db.Find(&settings)
	if result.Error != nil {
		return nil, result.Error
	}

	settingsMap := make(map[string]string)
	for _, setting := range settings {
		settingsMap[setting.Key] = setting.Value
	}
	return settingsMap, nil
}

func (s *SettingsService) GetGoogleConfig() (googlephotos.Config, error) {
	clientID, _ := s.Get("google_client_id")
	clientSecret, _ := s.Get("google_client_secret")

	return googlephotos.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  "", // Will be set dynamically
		Scopes: []string{
			"https://www.googleapis.com/auth/photospicker.mediaitems.readonly",
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
		},
	}, nil
}

// GetAmbientConfig returns the Google Photos Ambient API credentials. They are
// separate settings from google_client_id/secret because the Ambient API
// requires an OAuth client of type "TVs and Limited Input devices", and because
// resources created with one client ID are inaccessible to another.
func (s *SettingsService) GetAmbientConfig() (photosambient.Config, error) {
	clientID, _ := s.Get("google_ambient_client_id")
	clientSecret, _ := s.Get("google_ambient_client_secret")

	if clientID == "" || clientSecret == "" {
		return photosambient.Config{}, errors.New("google ambient credentials not configured")
	}
	return photosambient.Config{ClientID: clientID, ClientSecret: clientSecret}, nil
}

func (s *SettingsService) GetGoogleCalendarConfig() (googlephotos.Config, error) {
	clientID, _ := s.Get("google_client_id")
	clientSecret, _ := s.Get("google_client_secret")

	return googlephotos.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  "", // Will be set dynamically
		Scopes: []string{
			"https://www.googleapis.com/auth/calendar.readonly",
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
		},
	}, nil
}
