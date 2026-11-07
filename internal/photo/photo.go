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

// FeedbackType identifies the kind of feedback recorded for a shown photo.
type FeedbackType string

const (
	FeedbackReject    FeedbackType = "reject"
	FeedbackRating    FeedbackType = "rating"
	FeedbackFaultyTag FeedbackType = "faulty_tag"
)

// Feedback contains the values provided for a photo feedback event.
type Feedback struct {
	Type   FeedbackType
	Rating int
	Tag    string
}

// FeedbackEvent records feedback for a photo selection.
type FeedbackEvent struct {
	Type    FeedbackType `json:"type"`
	Path    string       `json:"path"`
	ShownAt time.Time    `json:"shown_at"`
	Rating  int          `json:"rating,omitempty"`
	Tag     string       `json:"tag,omitempty"`
}

// EventRecorder persists successful photo selections and feedback.
type EventRecorder interface {
	Record(HistoryEntry) error
	RecordFeedback(FeedbackEvent) error
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
