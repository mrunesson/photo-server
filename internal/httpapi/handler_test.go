package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"photo-server/internal/filesystem"
	"photo-server/internal/httpapi"
	"photo-server/internal/photo"
)

type photoResponse struct {
	Path     string          `json:"path"`
	Album    string          `json:"album"`
	Metadata json.RawMessage `json:"metadata"`
	ImageURL string          `json:"image_url"`
	ShownAt  json.RawMessage `json:"shown_at"`
}

type historyResponse struct {
	Photos []photoResponse `json:"photos"`
}

func TestPhotoAPI(t *testing.T) {
	images := map[string][]byte{
		"cover.jpg":           {0xff, 0xd8, 0xff, 0xd9, 0x01},
		"trips/coast/one.jpg": {0xff, 0xd8, 0xff, 0xd9, 0x02},
		"trips/coast/two.jpg": {0xff, 0xd8, 0xff, 0xd9, 0x03},
	}
	server := newServer(t, images)
	client := server.Client()

	response, err := client.Get(server.URL + "/api/photos/next?album=trips%2Fcoast")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("next status = %d, want 200: %s", response.StatusCode, body)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("next Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
	}
	var selected photoResponse
	if err := json.NewDecoder(response.Body).Decode(&selected); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if selected.Album != "trips/coast" || selected.Path == "" || selected.ImageURL == "" {
		t.Fatalf("next response = %#v, want photo from trips/coast with image URL", selected)
	}
	if len(selected.ShownAt) != 0 {
		t.Fatalf("next response unexpectedly includes shown_at: %s", selected.ShownAt)
	}
	if len(selected.Metadata) == 0 || string(selected.Metadata) == "null" {
		t.Fatalf("next response missing metadata: %s", selected.Metadata)
	}

	imageResponse, err := client.Get(server.URL + selected.ImageURL)
	if err != nil {
		t.Fatal(err)
	}
	image, err := io.ReadAll(imageResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := imageResponse.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if imageResponse.StatusCode != http.StatusOK || string(image) != string(images[selected.Path]) {
		t.Fatalf("image response = (%d, %v), want selected image bytes", imageResponse.StatusCode, image)
	}
	if got := imageResponse.Header.Get("Content-Type"); !strings.HasPrefix(got, "image/jpeg") {
		t.Errorf("image Content-Type = %q, want image/jpeg", got)
	}
	if imageResponse.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("image X-Content-Type-Options = %q, want nosniff", imageResponse.Header.Get("X-Content-Type-Options"))
	}

	for range 11 {
		response, err := client.Get(server.URL + "/api/photos/next")
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			t.Fatalf("next status = %d, want 200", response.StatusCode)
		}
		_ = response.Body.Close()
	}
	historyHTTPResponse, err := client.Get(server.URL + "/api/photos/history")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = historyHTTPResponse.Body.Close() }()
	if historyHTTPResponse.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d, want 200", historyHTTPResponse.StatusCode)
	}
	var history historyResponse
	if err := json.NewDecoder(historyHTTPResponse.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	if len(history.Photos) != 10 {
		t.Fatalf("history length = %d, want 10", len(history.Photos))
	}
	if history.Photos[0].Path == "" || history.Photos[0].ImageURL == "" {
		t.Fatalf("history entry missing path or image URL: %#v", history.Photos[0])
	}
	if len(history.Photos[0].Metadata) == 0 || string(history.Photos[0].Metadata) == "null" {
		t.Fatalf("history entry missing metadata: %s", history.Photos[0].Metadata)
	}
	for index, item := range history.Photos {
		if len(item.ShownAt) == 0 {
			t.Fatalf("history entry %d missing shown_at", index)
		}
		var shownAt time.Time
		if err := json.Unmarshal(item.ShownAt, &shownAt); err != nil {
			t.Fatalf("history entry %d shown_at %s: %v", index, item.ShownAt, err)
		}
		if shownAt.Location() != time.UTC || !strings.HasSuffix(string(item.ShownAt), "Z\"") {
			t.Errorf("history entry %d shown_at = %s, want UTC RFC3339 timestamp", index, item.ShownAt)
		}
	}

	latestShownAt := string(history.Photos[0].ShownAt)
	rereadResponse, err := client.Get(server.URL + "/api/photos/history")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rereadResponse.Body.Close() }()
	var reread historyResponse
	if err := json.NewDecoder(rereadResponse.Body).Decode(&reread); err != nil {
		t.Fatal(err)
	}
	if len(reread.Photos) == 0 {
		t.Fatal("re-read history is empty")
	}
	if string(reread.Photos[0].ShownAt) != latestShownAt {
		t.Fatalf("re-read newest shown_at = %s, want stable value %s", reread.Photos[0].ShownAt, latestShownAt)
	}
}

func TestPhotoAPIErrorsAndMethods(t *testing.T) {
	server := newServer(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}})
	client := server.Client()
	tests := []struct {
		name       string
		method     string
		url        string
		wantStatus int
	}{
		{name: "unknown album", method: http.MethodGet, url: "/api/photos/next?album=missing", wantStatus: http.StatusNotFound},
		{name: "invalid album", method: http.MethodGet, url: "/api/photos/next?album=..%2Fprivate", wantStatus: http.StatusBadRequest},
		{name: "invalid image id", method: http.MethodGet, url: "/api/photos/not-base64/image", wantStatus: http.StatusNotFound},
		{name: "unknown endpoint", method: http.MethodGet, url: "/unknown", wantStatus: http.StatusNotFound},
		{name: "wrong method", method: http.MethodPost, url: "/api/photos/next", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, err := http.NewRequest(tt.method, server.URL+tt.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusMethodNotAllowed && !strings.Contains(response.Header.Get("Allow"), http.MethodGet) {
				t.Errorf("Allow = %q, want GET", response.Header.Get("Allow"))
			}
		})
	}
}

func TestPhotoAPIEmptyCatalog(t *testing.T) {
	server := newServer(t, nil)
	response, err := server.Client().Get(server.URL + "/api/photos/next")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

func TestPhotoAPIDisappearedSelectionIsNotRecorded(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(httpapi.NewHandler(photo.NewService(disappearingSource{}, nil), logger))
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/api/photos/next")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("next status = %d, want 404", response.StatusCode)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	historyHTTPResponse, err := server.Client().Get(server.URL + "/api/photos/history")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = historyHTTPResponse.Body.Close() }()
	var history historyResponse
	if err := json.NewDecoder(historyHTTPResponse.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	if len(history.Photos) != 0 {
		t.Fatalf("history length = %d, want 0 after failed selection", len(history.Photos))
	}
}

func TestPhotoAPIEventLogFailureDoesNotRecordSelection(t *testing.T) {
	server := newServerWithRecorder(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}}, failingRecorder{})
	client := server.Client()

	response, err := client.Get(server.URL + "/api/photos/next")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("next status = %d, want 500", response.StatusCode)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	historyHTTPResponse, err := client.Get(server.URL + "/api/photos/history")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = historyHTTPResponse.Body.Close() }()
	var history historyResponse
	if err := json.NewDecoder(historyHTTPResponse.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	if len(history.Photos) != 0 {
		t.Fatalf("history length after failed event write = %d, want 0", len(history.Photos))
	}
}

type failingRecorder struct{}

func (failingRecorder) Record(photo.HistoryEntry) error {
	return errors.New("event log unavailable")
}

type disappearingSource struct{}

func (disappearingSource) List(context.Context) ([]photo.Photo, error) {
	return []photo.Photo{{Path: "removed.jpg"}}, nil
}

func (disappearingSource) Open(context.Context, string) (photo.Image, error) {
	return photo.Image{}, photo.ErrPhotoNotFound
}

func newServer(t *testing.T, images map[string][]byte) *httptest.Server {
	return newServerWithRecorder(t, images, nil)
}

func newServerWithRecorder(t *testing.T, images map[string][]byte, recorder photo.EventRecorder) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	for relativePath, contents := range images {
		fullPath := filepath.Join(root, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := filesystem.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := catalog.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(httpapi.NewHandler(photo.NewService(catalog, recorder), logger))
	t.Cleanup(server.Close)
	return server
}
