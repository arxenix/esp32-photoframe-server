// Package photosambient is a client for the Google Photos Ambient API
// (photosambient.googleapis.com), the API purpose-built for photo frames: the
// app registers a device in the user's Google Photos account, the user picks
// which albums feed it from inside the Google Photos app, and the app lists the
// resulting media items.
//
// It is deliberately separate from pkg/googlephotos (Picker API): the Ambient
// API needs its own OAuth client of type "TVs and Limited Input devices" and
// authorizes through the device-code flow instead of a browser redirect.
package photosambient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	// DefaultBaseURL is the Ambient API service endpoint.
	DefaultBaseURL = "https://photosambient.googleapis.com"

	// Scope is the OAuth scope every Ambient API method requires.
	Scope = "https://www.googleapis.com/auth/photosambient.mediaitems"

	// MaxPageSize is the largest page mediaItems.list honors.
	MaxPageSize = 100

	// DailyListQuota is Google's per-device, per-day cap on mediaItems.list
	// requests. Callers must budget their polling against it.
	DailyListQuota = 240

	// HighlightsSourceID is the one media source that cannot be listed with a
	// mediaSourceId filter (doing so returns INVALID_ARGUMENT); its items only
	// come back through the curated, unfiltered listing.
	HighlightsSourceID = "highlights"

	deviceAuthURL = "https://oauth2.googleapis.com/device/code"
	tokenURL      = "https://oauth2.googleapis.com/token"
	userInfoURL   = "https://www.googleapis.com/oauth2/v2/userinfo"
)

// Config holds the ambient OAuth client credentials ("TVs and Limited Input
// devices" application type).
type Config struct {
	ClientID     string
	ClientSecret string
}

// AmbientDevice is a device registered in the user's Google Photos account.
type AmbientDevice struct {
	ID              string         `json:"id,omitempty"`
	DisplayName     string         `json:"displayName,omitempty"`
	MediaSources    []MediaSource  `json:"mediaSources,omitempty"`
	SettingsURI     string         `json:"settingsUri,omitempty"`
	CreateTime      string         `json:"createTime,omitempty"`
	PollingConfig   *PollingConfig `json:"pollingConfig,omitempty"`
	MediaSourcesSet bool           `json:"mediaSourcesSet,omitempty"`
}

// MediaSource is an album or collection the user selected for a device.
type MediaSource struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// PollingConfig carries Google's recommended devices.get polling interval.
type PollingConfig struct {
	PollInterval string `json:"pollInterval"` // duration, e.g. "3.5s"
}

// Interval parses PollInterval ("30s") into a Duration; zero if unset/invalid.
func (p *PollingConfig) Interval() time.Duration {
	if p == nil {
		return 0
	}
	secs, err := strconv.ParseFloat(strings.TrimSuffix(p.PollInterval, "s"), 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// MediaItem is one photo selected for ambient display. ID is persistent across
// sessions, so it is usable as a dedup key.
type MediaItem struct {
	ID         string    `json:"id"`
	CreateTime string    `json:"createTime"`
	MediaFile  MediaFile `json:"mediaFile"`
}

// Created parses CreateTime, the time the photo was taken.
func (m MediaItem) Created() *time.Time {
	t, err := time.Parse(time.RFC3339, m.CreateTime)
	if err != nil {
		return nil
	}
	return &t
}

// IsImage reports whether this item is a still image (videos are not supported
// by the frame pipeline).
func (m MediaItem) IsImage() bool {
	return strings.HasPrefix(m.MediaFile.MimeType, "image/")
}

// DownloadURL returns the base URL with the mandatory size parameters applied,
// bounding the download to at most w x h while preserving aspect ratio.
func (m MediaItem) DownloadURL(w, h int) string {
	if m.MediaFile.BaseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s=w%d-h%d", m.MediaFile.BaseURL, w, h)
}

// MediaFile locates the bytes of a media item. BaseURL expires after about 60
// minutes, so bytes must be fetched during the sync that listed them.
type MediaFile struct {
	BaseURL           string            `json:"baseUrl"`
	MimeType          string            `json:"mimeType"`
	MediaFileMetadata MediaFileMetadata `json:"mediaFileMetadata"`
}

// MediaFileMetadata carries the dimensions of the original file.
type MediaFileMetadata struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// ListMediaItemsResponse is one page of a device's media items.
type ListMediaItemsResponse struct {
	MediaItems    []MediaItem `json:"mediaItems"`
	NextPageToken string      `json:"nextPageToken"`
}

// APIError is a non-2xx Ambient API response, carrying Google's canonical
// status so callers can react to the documented failure modes.
type APIError struct {
	HTTPStatus int
	Status     string // e.g. "ALREADY_EXISTS", "FAILED_PRECONDITION"
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ambient api: %s (%d): %s", e.Status, e.HTTPStatus, e.Message)
}

// IsStatus reports whether err is an APIError with the given canonical status.
func IsStatus(err error, status string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// NewRequestID returns a random UUIDv4 usable as a devices.create requestId. It
// carries no user-identifying information, as the API requires.
func NewRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

// OAuthConfig builds the device-flow OAuth config for the ambient credentials.
// The profile/email scopes let the UI show which account a frame is linked to.
func OAuthConfig(cfg Config) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Scopes: []string{
			Scope,
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
		},
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: deviceAuthURL,
			TokenURL:      tokenURL,
		},
	}
}

// RequestDeviceAuth starts the device-code flow. requestID/displayName are sent
// in the state parameter, which makes Google redirect the user straight to the
// new device's photo-selection screen once they authorize — the "streamlined"
// single-QR-code flow. The settingsUri is still worth showing as a fallback.
func RequestDeviceAuth(ctx context.Context, cfg Config, requestID, displayName string) (*oauth2.DeviceAuthResponse, error) {
	state, err := json.Marshal(struct {
		RequestID   string `json:"requestId"`
		DisplayName string `json:"displayName,omitempty"`
	}{RequestID: requestID, DisplayName: displayName})
	if err != nil {
		return nil, err
	}
	return OAuthConfig(cfg).DeviceAuth(ctx, oauth2.SetAuthURLParam("state", string(state)))
}

// WaitForToken polls Google until the user authorizes the frame (or the code
// expires / is denied). It blocks for up to the device code's lifetime.
func WaitForToken(ctx context.Context, cfg Config, auth *oauth2.DeviceAuthResponse) (*oauth2.Token, error) {
	return OAuthConfig(cfg).DeviceAccessToken(ctx, auth)
}

// Client calls the Ambient API with an authorized HTTP client.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient wraps an OAuth-authorized HTTP client.
func NewClient(httpClient *http.Client) *Client {
	return &Client{httpClient: httpClient, baseURL: DefaultBaseURL}
}

// WithBaseURL overrides the service endpoint (tests).
func (c *Client) WithBaseURL(base string) *Client {
	c.baseURL = strings.TrimSuffix(base, "/")
	return c
}

// AccountEmail returns the email of the authorized Google account.
func (c *Client) AccountEmail(ctx context.Context) (string, error) {
	var info struct {
		Email string `json:"email"`
	}
	if err := c.do(ctx, http.MethodGet, userInfoURL, nil, &info); err != nil {
		return "", err
	}
	return info.Email, nil
}

// CreateDevice registers a device in the user's Google Photos account. Pass the
// same requestID that was used for the device-code request so the streamlined
// flow lands the user on this device's settings page; retrying with it returns
// ALREADY_EXISTS instead of creating a duplicate.
func (c *Client) CreateDevice(ctx context.Context, requestID, displayName string) (*AmbientDevice, error) {
	u := fmt.Sprintf("%s/v1/devices?requestId=%s", c.baseURL, url.QueryEscape(requestID))
	var out AmbientDevice
	if err := c.do(ctx, http.MethodPost, u, AmbientDevice{DisplayName: displayName}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDevice retrieves a device, including whether the user has picked media
// sources for it yet.
func (c *Client) GetDevice(ctx context.Context, deviceID string) (*AmbientDevice, error) {
	u := fmt.Sprintf("%s/v1/devices/%s", c.baseURL, url.PathEscape(deviceID))
	var out AmbientDevice
	if err := c.do(ctx, http.MethodGet, u, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RenameDevice updates the display name shown in the Google Photos app.
func (c *Client) RenameDevice(ctx context.Context, deviceID, displayName string) (*AmbientDevice, error) {
	u := fmt.Sprintf("%s/v1/devices/%s", c.baseURL, url.PathEscape(deviceID))
	var out AmbientDevice
	if err := c.do(ctx, http.MethodPatch, u, AmbientDevice{DisplayName: displayName}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteDevice removes a device from the user's account. idOrRequestID accepts
// either the device id or the requestId it was created with, which is how an
// orphaned device (one whose id we never persisted) gets cleaned up.
func (c *Client) DeleteDevice(ctx context.Context, idOrRequestID string) error {
	u := fmt.Sprintf("%s/v1/devices/%s", c.baseURL, url.PathEscape(idOrRequestID))
	return c.do(ctx, http.MethodDelete, u, nil, nil)
}

// ListMediaItemsOptions selects what one mediaItems.list call returns.
type ListMediaItemsOptions struct {
	DeviceID string
	// MediaSourceID restricts the page to one of the device's media sources.
	// Empty means the curated ambient feed across all of them, whose pages may
	// repeat items already returned.
	MediaSourceID string
	PageSize      int
	PageToken     string
}

// ListMediaItems returns one page of the device's media items. It returns a
// FAILED_PRECONDITION APIError while the user has not configured any media
// sources for the device.
func (c *Client) ListMediaItems(ctx context.Context, opts ListMediaItemsOptions) (*ListMediaItemsResponse, error) {
	q := url.Values{}
	q.Set("deviceId", opts.DeviceID)
	if opts.MediaSourceID != "" {
		q.Set("mediaSourceId", opts.MediaSourceID)
	}
	if opts.PageSize > 0 {
		q.Set("pageSize", strconv.Itoa(min(opts.PageSize, MaxPageSize)))
	}
	if opts.PageToken != "" {
		q.Set("pageToken", opts.PageToken)
	}
	u := fmt.Sprintf("%s/v1/mediaItems?%s", c.baseURL, q.Encode())
	var out ListMediaItemsResponse
	if err := c.do(ctx, http.MethodGet, u, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// do performs a JSON request, decoding into out (may be nil) and translating
// error responses into *APIError.
func (c *Client) do(ctx context.Context, method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseAPIError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func parseAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var envelope struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	msg := envelope.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	return &APIError{HTTPStatus: resp.StatusCode, Status: envelope.Error.Status, Message: msg}
}
