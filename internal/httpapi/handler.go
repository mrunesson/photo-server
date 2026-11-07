// Package httpapi exposes photo selection, image delivery, and history over HTTP.
package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"photo-server/internal/photo"
)

const maxEncodedPhotoPath = 5464

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

// NewHandler creates the HTTP routes used by slideshow clients.
func NewHandler(service *photo.Service, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h := &handler{service: service, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/photos/next", h.next)
	mux.HandleFunc("GET /api/photos/history", h.history)
	mux.HandleFunc("GET /api/photos/{id}/image", h.image)
	mux.HandleFunc("/", h.notFound)
	return mux
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
	id := r.PathValue("id")
	if id == "" || len(id) > maxEncodedPhotoPath {
		writeError(w, http.StatusNotFound, "photo not found")
		return
	}
	relativePath, err := base64.RawURLEncoding.Strict().DecodeString(id)
	if err != nil || len(relativePath) == 0 || len(relativePath) > 4096 || base64.RawURLEncoding.EncodeToString(relativePath) != id {
		writeError(w, http.StatusNotFound, "photo not found")
		return
	}
	image, err := h.service.OpenImage(r.Context(), string(relativePath))
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
	http.ServeContent(w, r, path.Base(string(relativePath)), image.ModTime, image.Content)
}

func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/photos/next" || r.URL.Path == "/api/photos/history" ||
		(strings.HasPrefix(r.URL.Path, "/api/photos/") && strings.HasSuffix(r.URL.Path, "/image")) {
		w.Header().Set("Allow", "GET, HEAD")
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
