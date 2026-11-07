// Package httpapi exposes photo selection, image delivery, history, and feedback over HTTP.
package httpapi

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	swaggerui "github.com/swaggest/swgui/v5emb"

	"photo-server/internal/photo"
)

const maxEncodedPhotoPath = 5464
const maxFeedbackBodyBytes = 4096

//go:embed openapi.json
var openAPISpec []byte

type handler struct {
	service *photo.Service
	logger  *slog.Logger
}

type photoResponse struct {
	photo.Photo
	ImageURL string `json:"image_url"`
}

type historyResponse struct {
	Photos []historyPhotoResponse `json:"photos"`
}

type historyPhotoResponse struct {
	photoResponse
	ShownAt time.Time `json:"shown_at"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type ratingRequest struct {
	Rating int `json:"rating"`
}

type faultyTagRequest struct {
	Tag string `json:"tag"`
}

// NewHandler creates the HTTP routes used by slideshow clients.
func NewHandler(service *photo.Service, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h := &handler{service: service, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /docs", h.docsRedirect)
	mux.HandleFunc("GET /docs/openapi.json", h.openAPI)
	mux.Handle("GET /docs/", swaggerui.New("Photo Server API", "/docs/openapi.json", "/docs/"))
	mux.HandleFunc("GET /api/photos/next", h.next)
	mux.HandleFunc("GET /api/photos/history", h.history)
	mux.HandleFunc("GET /api/photos/{id}/image", h.image)
	mux.HandleFunc("POST /api/photos/{id}/reject", h.reject)
	mux.HandleFunc("POST /api/photos/{id}/rating", h.rate)
	mux.HandleFunc("POST /api/photos/{id}/faulty-tag", h.faultyTag)
	mux.HandleFunc("/", h.notFound)
	return mux
}

func (h *handler) docsRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
}

func (h *handler) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPISpec)
}

func (h *handler) next(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if values, ok := query["album"]; ok && len(values) != 1 {
		writeError(w, http.StatusBadRequest, "invalid album")
		return
	}
	selected, err := h.service.Next(r.Context(), query.Get("album"))
	if err != nil {
		switch {
		case errors.Is(err, photo.ErrInvalidAlbum):
			writeError(w, http.StatusBadRequest, "invalid album")
		case errors.Is(err, photo.ErrNoPhotos):
			writeError(w, http.StatusNotFound, "no photos found")
		case errors.Is(err, photo.ErrPhotoNotFound):
			writeError(w, http.StatusNotFound, "photo is no longer available")
		default:
			h.logger.Error("select photo", "error", err)
			writeError(w, http.StatusInternalServerError, "photo selection failed")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, makePhotoResponse(selected))
}

func (h *handler) history(w http.ResponseWriter, _ *http.Request) {
	selected := h.service.History()
	response := historyResponse{Photos: make([]historyPhotoResponse, len(selected))}
	for index, item := range selected {
		response.Photos[index] = historyPhotoResponse{
			photoResponse: makePhotoResponse(item.Photo),
			ShownAt:       item.ShownAt,
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (h *handler) image(w http.ResponseWriter, r *http.Request) {
	relativePath, ok := decodePhotoID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "photo not found")
		return
	}
	image, err := h.service.OpenImage(r.Context(), relativePath)
	if err != nil {
		if errors.Is(err, photo.ErrPhotoNotFound) {
			writeError(w, http.StatusNotFound, "photo not found")
			return
		}
		h.logger.Error("open photo", "error", err)
		writeError(w, http.StatusInternalServerError, "photo delivery failed")
		return
	}
	defer func() {
		if err := image.Content.Close(); err != nil {
			h.logger.Warn("close photo", "error", err)
		}
	}()
	w.Header().Set("Content-Type", image.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, path.Base(relativePath), image.ModTime, image.Content)
}

func (h *handler) reject(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFeedbackBodyBytes))
	if err != nil || len(body) != 0 {
		writeError(w, http.StatusBadRequest, "reject request must not contain a body")
		return
	}
	h.recordFeedback(w, r, photo.Feedback{Type: photo.FeedbackReject})
}

func (h *handler) rate(w http.ResponseWriter, r *http.Request) {
	var request ratingRequest
	if err := decodeFeedbackBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid rating request")
		return
	}
	h.recordFeedback(w, r, photo.Feedback{Type: photo.FeedbackRating, Rating: request.Rating})
}

func (h *handler) faultyTag(w http.ResponseWriter, r *http.Request) {
	var request faultyTagRequest
	if err := decodeFeedbackBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid faulty-tag request")
		return
	}
	h.recordFeedback(w, r, photo.Feedback{Type: photo.FeedbackFaultyTag, Tag: request.Tag})
}

func (h *handler) recordFeedback(w http.ResponseWriter, r *http.Request, feedback photo.Feedback) {
	relativePath, ok := decodePhotoID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "photo not found")
		return
	}
	if err := h.service.RecordFeedback(relativePath, feedback); err != nil {
		switch {
		case errors.Is(err, photo.ErrInvalidFeedback):
			writeError(w, http.StatusBadRequest, "invalid photo feedback")
		case errors.Is(err, photo.ErrPhotoNotInHistory):
			writeError(w, http.StatusNotFound, "photo was not shown recently")
		case errors.Is(err, photo.ErrFeedbackRecorderUnavailable):
			writeError(w, http.StatusServiceUnavailable, "feedback recording unavailable")
		default:
			h.logger.Error("record photo feedback", "error", err)
			writeError(w, http.StatusInternalServerError, "photo feedback failed")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodePhotoID(id string) (string, bool) {
	if id == "" || len(id) > maxEncodedPhotoPath {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(id)
	if err != nil || len(decoded) == 0 || len(decoded) > 4096 || base64.RawURLEncoding.EncodeToString(decoded) != id {
		return "", false
	}
	return string(decoded), true
}

func decodeFeedbackBody(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFeedbackBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/docs" || strings.HasPrefix(r.URL.Path, "/docs/") {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.Path == "/api/photos/next" || r.URL.Path == "/api/photos/history" ||
		(strings.HasPrefix(r.URL.Path, "/api/photos/") && (strings.HasSuffix(r.URL.Path, "/image") ||
			strings.HasSuffix(r.URL.Path, "/reject") || strings.HasSuffix(r.URL.Path, "/rating") || strings.HasSuffix(r.URL.Path, "/faulty-tag"))) {
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeError(w, http.StatusNotFound, "endpoint not found")
}

func makePhotoResponse(selected photo.Photo) photoResponse {
	id := base64.RawURLEncoding.EncodeToString([]byte(selected.Path))
	return photoResponse{
		Photo:    selected,
		ImageURL: "/api/photos/" + id + "/image",
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: strings.TrimSpace(message)})
}
