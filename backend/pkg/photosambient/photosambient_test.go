package photosambient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRequestIDIsUUIDv4(t *testing.T) {
	id, err := NewRequestID()
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`), id)

	other, err := NewRequestID()
	require.NoError(t, err)
	assert.NotEqual(t, id, other)
}

func TestCreateDevicePassesRequestID(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"dev1","displayName":"Frame","settingsUri":"https://photos.google.com/x","pollingConfig":{"pollInterval":"30s"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.Client()).WithBaseURL(srv.URL)
	dev, err := c.CreateDevice(context.Background(), "11111111-2222-4333-8444-555555555555", "Frame")
	require.NoError(t, err)

	assert.Equal(t, "/v1/devices", gotPath)
	assert.Equal(t, "requestId=11111111-2222-4333-8444-555555555555", gotQuery)
	assert.JSONEq(t, `{"displayName":"Frame"}`, gotBody)
	assert.Equal(t, "dev1", dev.ID)
	assert.Equal(t, 30*time.Second, dev.PollingConfig.Interval())
}

func TestCreateDeviceAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":409,"status":"ALREADY_EXISTS","message":"device exists"}}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.Client()).WithBaseURL(srv.URL).
		CreateDevice(context.Background(), "req", "Frame")
	require.Error(t, err)
	assert.True(t, IsStatus(err, "ALREADY_EXISTS"))
	assert.False(t, IsStatus(err, "NOT_FOUND"))
}

func TestListMediaItemsQueryAndParsing(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Encode()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mediaItems":[
			{"id":"m1","createTime":"2014-10-02T15:01:23Z","mediaFile":{"baseUrl":"https://lh3/p/a","mimeType":"image/jpeg","mediaFileMetadata":{"width":4032,"height":3024}}},
			{"id":"m2","mediaFile":{"baseUrl":"https://lh3/p/b","mimeType":"video/mp4"}}
		],"nextPageToken":"tok2"}`))
	}))
	defer srv.Close()

	resp, err := NewClient(srv.Client()).WithBaseURL(srv.URL).
		ListMediaItems(context.Background(), ListMediaItemsOptions{
			DeviceID: "dev1", MediaSourceID: "src1", PageSize: 500, PageToken: "tok1",
		})
	require.NoError(t, err)

	// pageSize is clamped to the API maximum.
	assert.Equal(t, "deviceId=dev1&mediaSourceId=src1&pageSize=100&pageToken=tok1", gotQuery)
	assert.Equal(t, "tok2", resp.NextPageToken)
	require.Len(t, resp.MediaItems, 2)

	photo := resp.MediaItems[0]
	assert.True(t, photo.IsImage())
	assert.Equal(t, 4032, photo.MediaFile.MediaFileMetadata.Width)
	assert.Equal(t, "https://lh3/p/a=w1600-h1600", photo.DownloadURL(1600, 1600))
	require.NotNil(t, photo.Created())
	assert.Equal(t, 2014, photo.Created().Year())

	video := resp.MediaItems[1]
	assert.False(t, video.IsImage())
	assert.Nil(t, video.Created())
}

func TestListMediaItemsFailedPrecondition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"status":"FAILED_PRECONDITION","message":"no media sources"}}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.Client()).WithBaseURL(srv.URL).
		ListMediaItems(context.Background(), ListMediaItemsOptions{DeviceID: "dev1"})
	require.Error(t, err)
	assert.True(t, IsStatus(err, "FAILED_PRECONDITION"))
	assert.Contains(t, err.Error(), "no media sources")
}

func TestDeleteAndRenameDevice(t *testing.T) {
	var methods, paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"dev1","displayName":"Living room"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.Client()).WithBaseURL(srv.URL)
	dev, err := c.RenameDevice(context.Background(), "dev1", "Living room")
	require.NoError(t, err)
	assert.Equal(t, "Living room", dev.DisplayName)
	require.NoError(t, c.DeleteDevice(context.Background(), "dev1"))

	assert.Equal(t, []string{http.MethodPatch, http.MethodDelete}, methods)
	assert.Equal(t, []string{"/v1/devices/dev1", "/v1/devices/dev1"}, paths)
}

func TestPollingConfigInterval(t *testing.T) {
	assert.Equal(t, time.Duration(0), (*PollingConfig)(nil).Interval())
	assert.Equal(t, time.Duration(0), (&PollingConfig{}).Interval())
	assert.Equal(t, 3500*time.Millisecond, (&PollingConfig{PollInterval: "3.5s"}).Interval())
}
