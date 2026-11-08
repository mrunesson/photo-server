package httpapi_test

import (
	"bufio"
	"context"
	"encoding/base64"
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

	"photo-server/internal/eventlog"
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

func TestOpenAPIDocument(t *testing.T) {
	server := newServer(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}})
	client := server.Client()

	response, err := client.Get(server.URL + "/docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("OpenAPI status = %d, want 200: %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("OpenAPI Content-Type = %q, want application/json; charset=utf-8", got)
	}

	var document struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode OpenAPI document: %v", err)
	}
	if document.OpenAPI != "3.0.3" || document.Info.Title == "" || document.Info.Version == "" {
		t.Fatalf("OpenAPI metadata = (%q, %q, %q), want version 3.0.3 and non-empty title/version", document.OpenAPI, document.Info.Title, document.Info.Version)
	}
	if _, ok := document.Paths["/docs/openapi.json"]; !ok {
		t.Error("OpenAPI paths missing /docs/openapi.json")
	}
	if _, ok := document.Paths["/openapi.json"]; ok {
		t.Error("OpenAPI paths still include the old /openapi.json route")
	}

	operations := map[string]struct {
		method    string
		responses []string
		bodyRef   string
	}{
		"/api/photos/next":            {method: "get", responses: []string{"200", "400", "404", "500"}},
		"/api/photos/events":          {method: "get", responses: []string{"200", "405", "500"}},
		"/api/photos/history":         {method: "get", responses: []string{"200"}},
		"/api/photos/{id}/image":      {method: "get", responses: []string{"200", "206", "304", "404", "416", "500"}},
		"/api/photos/{id}/reject":     {method: "post", responses: []string{"204", "400", "404", "500", "503"}},
		"/api/photos/{id}/rating":     {method: "post", responses: []string{"204", "400", "404", "500", "503"}, bodyRef: "#/components/schemas/RatingRequest"},
		"/api/photos/{id}/faulty-tag": {method: "post", responses: []string{"204", "400", "404", "500", "503"}, bodyRef: "#/components/schemas/FaultyTagRequest"},
	}
	for path, expected := range operations {
		pathItem, ok := document.Paths[path]
		if !ok {
			t.Errorf("OpenAPI paths missing %s", path)
			continue
		}
		var operation struct {
			Responses   map[string]json.RawMessage `json:"responses"`
			RequestBody *struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		}
		if raw, ok := pathItem[expected.method]; !ok {
			t.Errorf("OpenAPI %s %s operation missing", strings.ToUpper(expected.method), path)
			continue
		} else if err := json.Unmarshal(raw, &operation); err != nil {
			t.Errorf("decode OpenAPI %s %s operation: %v", strings.ToUpper(expected.method), path, err)
			continue
		}
		for _, status := range expected.responses {
			if _, ok := operation.Responses[status]; !ok {
				t.Errorf("OpenAPI %s %s response %s missing", strings.ToUpper(expected.method), path, status)
			}
		}
		if expected.bodyRef != "" {
			if operation.RequestBody == nil {
				t.Errorf("OpenAPI %s %s request body missing", strings.ToUpper(expected.method), path)
				continue
			}
			content, ok := operation.RequestBody.Content["application/json"]
			if !ok || content.Schema.Ref != expected.bodyRef {
				t.Errorf("OpenAPI %s %s JSON request schema = %q, want %q", strings.ToUpper(expected.method), path, content.Schema.Ref, expected.bodyRef)
			}
		}
	}

	type schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	decodeSchema := func(name string) schema {
		t.Helper()
		var value schema
		if raw, ok := document.Components.Schemas[name]; !ok {
			t.Errorf("OpenAPI component schema %q missing", name)
		} else if err := json.Unmarshal(raw, &value); err != nil {
			t.Errorf("decode OpenAPI schema %q: %v", name, err)
		}
		return value
	}
	rating := decodeSchema("RatingRequest")
	if len(rating.Required) != 1 || rating.Required[0] != "rating" {
		t.Errorf("rating required fields = %v, want [rating]", rating.Required)
	}
	var ratingField struct {
		Type    string `json:"type"`
		Minimum int    `json:"minimum"`
		Maximum int    `json:"maximum"`
	}
	if err := json.Unmarshal(rating.Properties["rating"], &ratingField); err != nil || ratingField.Type != "integer" || ratingField.Minimum != 1 || ratingField.Maximum != 5 {
		t.Errorf("rating schema = %#v (decode error %v), want integer from 1 to 5", ratingField, err)
	}
	faultyTag := decodeSchema("FaultyTagRequest")
	if len(faultyTag.Required) != 1 || faultyTag.Required[0] != "tag" {
		t.Errorf("faulty-tag required fields = %v, want [tag]", faultyTag.Required)
	}
	metadata := decodeSchema("Metadata")
	var capturedAt struct {
		Type   string `json:"type"`
		Format string `json:"format"`
	}
	if err := json.Unmarshal(metadata.Properties["captured_at"], &capturedAt); err != nil || capturedAt.Type != "string" || capturedAt.Format != "" {
		t.Errorf("captured_at schema = %#v (decode error %v), want a string without a date-time format", capturedAt, err)
	}
	for _, name := range []string{"PhotoResponse", "HistoryResponse", "HistoryPhoto", "Coordinates", "ErrorResponse"} {
		decodeSchema(name)
	}

	headResponse, err := client.Head(server.URL + "/docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := headResponse.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if headResponse.StatusCode != http.StatusOK || headResponse.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("OpenAPI HEAD response = (%d, %q), want (200, application/json; charset=utf-8)", headResponse.StatusCode, headResponse.Header.Get("Content-Type"))
	}

	unsupportedResponse, err := client.Post(server.URL+"/docs/openapi.json", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = unsupportedResponse.Body.Close()
	if unsupportedResponse.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("OpenAPI POST status = %d, want 405", unsupportedResponse.StatusCode)
	}
	if got := unsupportedResponse.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("OpenAPI POST Allow = %q, want GET, HEAD", got)
	}

	oldEndpoint, err := client.Get(server.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = oldEndpoint.Body.Close()
	if oldEndpoint.StatusCode != http.StatusNotFound {
		t.Errorf("old OpenAPI endpoint status = %d, want 404", oldEndpoint.StatusCode)
	}
}

func TestPhotoEventsAPIStreamsSuccessfulSelections(t *testing.T) {
	server := newServer(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}})
	firstResponse, firstEvents, cancelFirst := openPhotoEvents(t, server, 5*time.Second)
	defer cancelFirst()
	defer func() { _ = firstResponse.Body.Close() }()
	secondResponse, secondEvents, cancelSecond := openPhotoEvents(t, server, 5*time.Second)
	defer cancelSecond()
	defer func() { _ = secondResponse.Body.Close() }()

	for index := range 2 {
		selected := requestSelectedPhoto(t, server)
		for subscriberIndex, events := range []*bufio.Reader{firstEvents, secondEvents} {
			got, err := readPhotoEvent(events)
			if err != nil {
				t.Fatalf("read subscriber %d event: %v", subscriberIndex, err)
			}
			if got.Path != selected.Path || got.ImageURL != selected.ImageURL || len(got.ShownAt) == 0 {
				t.Errorf("subscriber %d event %d = %#v, want selection %#v with shown_at", subscriberIndex, index, got, selected)
			}
		}
	}
}

func TestPhotoEventsAPIDoesNotPublishFailedSelections(t *testing.T) {
	server := newServerWithRecorder(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}}, &firstSelectionFailingRecorder{})
	streamResponse, events, cancel := openPhotoEvents(t, server, 5*time.Second)
	defer cancel()
	defer func() { _ = streamResponse.Body.Close() }()

	response, err := server.Client().Get(server.URL + "/api/photos/next")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("next status = %d, want 500 after event-log failure", response.StatusCode)
	}

	selected := requestSelectedPhoto(t, server)
	historyHTTPResponse, err := server.Client().Get(server.URL + "/api/photos/history")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = historyHTTPResponse.Body.Close() }()
	var history historyResponse
	if err := json.NewDecoder(historyHTTPResponse.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	if len(history.Photos) != 1 {
		t.Fatalf("history entries = %d, want one after successful selection", len(history.Photos))
	}

	got, err := readPhotoEvent(events)
	if err != nil {
		t.Fatalf("read successful selection event: %v", err)
	}
	var gotShownAt, wantShownAt time.Time
	if err := json.Unmarshal(got.ShownAt, &gotShownAt); err != nil {
		t.Fatalf("decode event shown_at: %v", err)
	}
	if err := json.Unmarshal(history.Photos[0].ShownAt, &wantShownAt); err != nil {
		t.Fatalf("decode history shown_at: %v", err)
	}
	if got.Path != selected.Path || !gotShownAt.Equal(wantShownAt) {
		t.Fatalf("stream event = %#v at %s, want successful selection %q at %s", got, gotShownAt, selected.Path, wantShownAt)
	}
}

func TestPhotoEventsAPIHeadReturnsWithoutStreaming(t *testing.T) {
	server := newServer(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}})
	request, err := http.NewRequest(http.MethodHead, server.URL+"/api/photos/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(request.Context(), time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("HEAD response = (%d, %q), want 200 text/event-stream", response.StatusCode, response.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatalf("HEAD body = %q, want empty", body)
	}
}

func openPhotoEvents(t *testing.T, server *httptest.Server, timeout time.Duration) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/photos/events", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("events status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("events Content-Type = %q, want text/event-stream", got)
	}
	reader := bufio.NewReader(response.Body)
	comment, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(comment) != ": connected" {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("initial stream comment = %q, error %v", comment, err)
	}
	blank, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(blank) != "" {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("initial stream separator = %q, error %v", blank, err)
	}
	return response, reader, cancel
}

func readPhotoEvent(reader *bufio.Reader) (photoResponse, error) {
	eventName := ""
	data := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return photoResponse{}, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if eventName == "" {
				continue
			}
			break
		}
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if eventName != "photo" {
		return photoResponse{}, errors.New("unexpected server-sent event type: " + eventName)
	}
	var selected photoResponse
	if err := json.Unmarshal([]byte(data), &selected); err != nil {
		return photoResponse{}, err
	}
	return selected, nil
}

func TestDocsUI(t *testing.T) {
	server := newServer(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}})
	client := server.Client()

	redirectClient := *client
	redirectClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	redirect, err := redirectClient.Get(server.URL + "/docs")
	if err != nil {
		t.Fatal(err)
	}
	_ = redirect.Body.Close()
	if redirect.StatusCode != http.StatusMovedPermanently || redirect.Header.Get("Location") != "/docs/" {
		t.Fatalf("/docs response = (%d, %q), want redirect to /docs/", redirect.StatusCode, redirect.Header.Get("Location"))
	}

	response, err := client.Get(server.URL + "/docs/")
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("docs UI response = (%d, %q), want HTML 200", response.StatusCode, response.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(page), "/docs/openapi.json") {
		t.Errorf("docs UI page does not configure /docs/openapi.json")
	}
	if strings.Contains(string(page), "cdn.jsdelivr.net") || strings.Contains(string(page), "unpkg.com") {
		t.Error("docs UI page references external CDN assets")
	}

	headResponse, err := client.Head(server.URL + "/docs/")
	if err != nil {
		t.Fatal(err)
	}
	headBody, readErr := io.ReadAll(headResponse.Body)
	_ = headResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if headResponse.StatusCode != http.StatusOK || len(headBody) != 0 {
		t.Errorf("docs UI HEAD response = (%d, %d bytes), want 200 with no body", headResponse.StatusCode, len(headBody))
	}

	for _, asset := range []string{"swagger-ui.css", "swagger-ui-bundle.js"} {
		assetResponse, err := client.Get(server.URL + "/docs/" + asset)
		if err != nil {
			t.Fatal(err)
		}
		assetBody, readErr := io.ReadAll(assetResponse.Body)
		_ = assetResponse.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if assetResponse.StatusCode != http.StatusOK || len(assetBody) == 0 {
			t.Errorf("docs asset %s response = (%d, %d bytes), want non-empty 200", asset, assetResponse.StatusCode, len(assetBody))
		}
	}

	missing, err := client.Get(server.URL + "/docs/not-a-real-asset.js")
	if err != nil {
		t.Fatal(err)
	}
	_ = missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing docs asset status = %d, want 404", missing.StatusCode)
	}

	unsupported, err := client.Post(server.URL+"/docs/", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = unsupported.Body.Close()
	if unsupported.StatusCode != http.StatusMethodNotAllowed || unsupported.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("docs UI POST response = (%d, %q), want 405 and Allow GET, HEAD", unsupported.StatusCode, unsupported.Header.Get("Allow"))
	}
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

func TestPhotoFeedbackAPIRecordsDurableEvents(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	recorder, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := recorder.Close(); err != nil {
			t.Errorf("close event log: %v", err)
		}
	})
	server := newServerWithRecorder(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}}, recorder)
	client := server.Client()

	nextResponse, err := client.Get(server.URL + "/api/photos/next")
	if err != nil {
		t.Fatal(err)
	}
	var selected photoResponse
	if err := json.NewDecoder(nextResponse.Body).Decode(&selected); err != nil {
		t.Fatal(err)
	}
	if err := nextResponse.Body.Close(); err != nil {
		t.Fatal(err)
	}
	baseURL := strings.TrimSuffix(selected.ImageURL, "/image")
	requests := []struct {
		name string
		path string
		body string
	}{
		{name: "reject", path: "/reject"},
		{name: "rating", path: "/rating", body: `{"rating":4}`},
		{name: "faulty tag", path: "/faulty-tag", body: `{"tag":"incorrect location"}`},
	}
	for _, tt := range requests {
		t.Run(tt.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL+baseURL+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if tt.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("feedback status = %d, want 204", response.StatusCode)
			}
		})
	}

	historyHTTPResponse, err := client.Get(server.URL + "/api/photos/history")
	if err != nil {
		t.Fatal(err)
	}
	var history historyResponse
	if err := json.NewDecoder(historyHTTPResponse.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	if err := historyHTTPResponse.Body.Close(); err != nil {
		t.Fatal(err)
	}
	var shownAt time.Time
	if err := json.Unmarshal(history.Photos[0].ShownAt, &shownAt); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	events := readLoggedEvents(t, logPath)
	wantTypes := []string{"", "reject", "rating", "faulty_tag"}
	if len(events) != len(wantTypes) {
		t.Fatalf("logged event count = %d, want %d", len(events), len(wantTypes))
	}
	for index, wantType := range wantTypes {
		if events[index].Type != wantType || events[index].Path != selected.Path || !events[index].ShownAt.Equal(shownAt) {
			t.Errorf("logged event %d = %#v, want type %q for latest selection", index, events[index], wantType)
		}
	}
	if events[2].Rating != 4 || events[3].Tag != "incorrect location" {
		t.Errorf("rating/tag events = %#v and %#v, want rating 4 and tag", events[2], events[3])
	}
}

func TestPhotoFeedbackAPIRejectsInvalidRequests(t *testing.T) {
	recorder := &countingRecorder{}
	server := newServerWithRecorder(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}}, recorder)
	client := server.Client()
	selected := requestSelectedPhoto(t, server)
	baseURL := strings.TrimSuffix(selected.ImageURL, "/image")
	tests := []struct {
		name       string
		path       string
		body       string
		wantStatus int
	}{
		{name: "malformed JSON", path: "/rating", body: `{"rating":`, wantStatus: http.StatusBadRequest},
		{name: "oversized JSON", path: "/rating", body: strings.Repeat(" ", 5000), wantStatus: http.StatusBadRequest},
		{name: "missing rating", path: "/rating", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "rating too low", path: "/rating", body: `{"rating":0}`, wantStatus: http.StatusBadRequest},
		{name: "rating too high", path: "/rating", body: `{"rating":6}`, wantStatus: http.StatusBadRequest},
		{name: "unknown rating field", path: "/rating", body: `{"rating":4,"extra":true}`, wantStatus: http.StatusBadRequest},
		{name: "missing tag", path: "/faulty-tag", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "empty tag", path: "/faulty-tag", body: `{"tag":"  "}`, wantStatus: http.StatusBadRequest},
		{name: "reject body", path: "/reject", body: `{}`, wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL+baseURL+tt.path, strings.NewReader(tt.body))
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
		})
	}
	if len(recorder.feedback) != 0 {
		t.Fatalf("feedback events = %d, want none for invalid requests", len(recorder.feedback))
	}

	invalidIDResponse, err := client.Post(server.URL+"/api/photos/not-base64/rating", "application/json", strings.NewReader(`{"rating":4}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = invalidIDResponse.Body.Close()
	if invalidIDResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid ID status = %d, want 404", invalidIDResponse.StatusCode)
	}
	unknownPhotoID := base64.RawURLEncoding.EncodeToString([]byte("not-shown.jpg"))
	unknownResponse, err := client.Post(server.URL+"/api/photos/"+unknownPhotoID+"/reject", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = unknownResponse.Body.Close()
	if unknownResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown history path status = %d, want 404", unknownResponse.StatusCode)
	}
	if len(recorder.feedback) != 0 {
		t.Fatalf("feedback events after invalid IDs = %d, want none", len(recorder.feedback))
	}

	wrongMethod, err := client.Get(server.URL + baseURL + "/reject")
	if err != nil {
		t.Fatal(err)
	}
	_ = wrongMethod.Body.Close()
	if wrongMethod.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d, want 405", wrongMethod.StatusCode)
	}
	if !strings.Contains(wrongMethod.Header.Get("Allow"), http.MethodPost) {
		t.Errorf("Allow = %q, want POST", wrongMethod.Header.Get("Allow"))
	}
}

func TestPhotoFeedbackAPIReportsUnavailableRecorder(t *testing.T) {
	server := newServer(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}})
	selected := requestSelectedPhoto(t, server)
	response, err := server.Client().Post(server.URL+strings.TrimSuffix(selected.ImageURL, "/image")+"/reject", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("feedback status = %d, want 503 without recorder", response.StatusCode)
	}
}

func TestPhotoFeedbackAPIReportsRecorderFailure(t *testing.T) {
	server := newServerWithRecorder(t, map[string][]byte{"photo.jpg": {0xff, 0xd8, 0xff, 0xd9}}, feedbackFailingRecorder{})
	selected := requestSelectedPhoto(t, server)
	response, err := server.Client().Post(server.URL+strings.TrimSuffix(selected.ImageURL, "/image")+"/reject", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("feedback status = %d, want 500 after recorder failure", response.StatusCode)
	}
}

type loggedEvent struct {
	Type    string    `json:"type"`
	Path    string    `json:"path"`
	ShownAt time.Time `json:"shown_at"`
	Rating  int       `json:"rating,omitempty"`
	Tag     string    `json:"tag,omitempty"`
}

func readLoggedEvents(t *testing.T, path string) []loggedEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var events []loggedEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event loggedEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode event %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func requestSelectedPhoto(t *testing.T, server *httptest.Server) photoResponse {
	t.Helper()
	response, err := server.Client().Get(server.URL + "/api/photos/next")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("next status = %d, want 200", response.StatusCode)
	}
	var selected photoResponse
	if err := json.NewDecoder(response.Body).Decode(&selected); err != nil {
		t.Fatal(err)
	}
	return selected
}

type countingRecorder struct {
	feedback []photo.FeedbackEvent
}

func (countingRecorder) Record(photo.HistoryEntry) error { return nil }

func (r *countingRecorder) RecordFeedback(event photo.FeedbackEvent) error {
	r.feedback = append(r.feedback, event)
	return nil
}

type feedbackFailingRecorder struct{}

func (feedbackFailingRecorder) Record(photo.HistoryEntry) error { return nil }

func (feedbackFailingRecorder) RecordFeedback(photo.FeedbackEvent) error {
	return errors.New("feedback log unavailable")
}

type failingRecorder struct{}

func (failingRecorder) Record(photo.HistoryEntry) error {
	return errors.New("event log unavailable")
}

func (failingRecorder) RecordFeedback(photo.FeedbackEvent) error {
	return errors.New("event log unavailable")
}

type firstSelectionFailingRecorder struct {
	failed bool
}

func (r *firstSelectionFailingRecorder) Record(photo.HistoryEntry) error {
	if !r.failed {
		r.failed = true
		return errors.New("event log unavailable")
	}
	return nil
}

func (*firstSelectionFailingRecorder) RecordFeedback(photo.FeedbackEvent) error {
	return nil
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
