// Package photo defines slideshow photo metadata and selection behavior.
package photo

import (
	"context"
	"io"
	"time"
)

// Coordinates identifies the location recorded in a photo's EXIF metadata.
type Coordinates struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// Metadata contains the EXIF values needed by slideshow clients.
type Metadata struct {
	CapturedAt  string            `json:"captured_at,omitempty"`
	Coordinates *Coordinates      `json:"coordinates,omitempty"`
	EXIF        map[string]string `json:"exif,omitempty"`
}

// Photo identifies a supported image relative to the configured photo root.
type Photo struct {
	Path     string   `json:"path"`
	Album    string   `json:"album,omitempty"`
	Metadata Metadata `json:"metadata"`
}

// HistoryEntry records a photo selection and when it was shown.
type HistoryEntry struct {
	Photo   Photo
	ShownAt time.Time
}

// EventRecorder persists successful photo selections.
type EventRecorder interface {
	Record(HistoryEntry) error
}

// Image provides a seekable photo file and the information needed for HTTP delivery.
type Image struct {
	Content     io.ReadSeekCloser
	ContentType string
	ModTime     time.Time
}

// Source discovers photos and opens a path that belongs to its catalog.
type Source interface {
	List(ctx context.Context) ([]Photo, error)
	Open(ctx context.Context, relativePath string) (Image, error)
}
