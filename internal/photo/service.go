package photo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

const historyLimit = 10

var (
	// ErrNoPhotos reports that the requested selection contains no available photos.
	ErrNoPhotos = errors.New("no photos found")
	// ErrInvalidAlbum reports that an album name is not a local catalog path.
	ErrInvalidAlbum = errors.New("invalid album")
	// ErrPhotoNotFound reports that a requested image is no longer in the photo catalog.
	ErrPhotoNotFound = errors.New("photo not found")
)

// Service selects catalog photos and records successful selections in memory.
type Service struct {
	source   Source
	recorder EventRecorder
	mu       sync.Mutex
	history  []HistoryEntry
}

// NewService creates a photo service backed by source and recorder. If recorder is nil, selections are recorded in memory only.
func NewService(source Source, recorder EventRecorder) *Service {
	return &Service{source: source, recorder: recorder}
}

// Next selects and verifies a photo, avoiding the previous photo when alternatives exist.
func (s *Service) Next(ctx context.Context, album string) (Photo, error) {
	if album != "" && (!fs.ValidPath(album) || path.Clean(album) != album || strings.Contains(album, "\\")) {
		return Photo{}, ErrInvalidAlbum
	}

	photos, err := s.source.List(ctx)
	if err != nil {
		return Photo{}, fmt.Errorf("discover photos: %w", err)
	}

	candidates := make([]Photo, 0, len(photos))
	for _, candidate := range photos {
		if album == "" || candidate.Album == album {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return Photo{}, ErrNoPhotos
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(candidates) > 1 && len(s.history) > 0 {
		previous := s.history[0].Photo.Path
		withoutPrevious := make([]Photo, 0, len(candidates)-1)
		for _, candidate := range candidates {
			if candidate.Path != previous {
				withoutPrevious = append(withoutPrevious, candidate)
			}
		}
		if len(withoutPrevious) > 0 {
			candidates = withoutPrevious
		}
	}

	selected := candidates[rand.IntN(len(candidates))]
	image, err := s.source.Open(ctx, selected.Path)
	if err != nil {
		return Photo{}, fmt.Errorf("verify selected photo: %w", err)
	}
	if err := image.Content.Close(); err != nil {
		return Photo{}, fmt.Errorf("close selected photo: %w", err)
	}

	entry := HistoryEntry{Photo: clonePhoto(selected), ShownAt: time.Now().UTC()}
	if s.recorder != nil {
		if err := s.recorder.Record(entry); err != nil {
			return Photo{}, fmt.Errorf("record photo selection: %w", err)
		}
	}
	s.history = append([]HistoryEntry{entry}, s.history...)
	if len(s.history) > historyLimit {
		s.history = s.history[:historyLimit]
	}
	return clonePhoto(selected), nil
}

// History returns up to ten successful selections, newest first.
func (s *Service) History() []HistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	history := make([]HistoryEntry, len(s.history))
	for index, selected := range s.history {
		history[index] = HistoryEntry{Photo: clonePhoto(selected.Photo), ShownAt: selected.ShownAt}
	}
	return history
}

// OpenImage opens a photo only if a fresh discovery still includes its relative path.
func (s *Service) OpenImage(ctx context.Context, relativePath string) (Image, error) {
	if !fs.ValidPath(relativePath) || path.Clean(relativePath) != relativePath {
		return Image{}, ErrPhotoNotFound
	}

	photos, err := s.source.List(ctx)
	if err != nil {
		return Image{}, fmt.Errorf("discover photos: %w", err)
	}
	if !slices.ContainsFunc(photos, func(candidate Photo) bool {
		return candidate.Path == relativePath
	}) {
		return Image{}, ErrPhotoNotFound
	}

	image, err := s.source.Open(ctx, relativePath)
	if err != nil {
		if errors.Is(err, ErrPhotoNotFound) {
			return Image{}, ErrPhotoNotFound
		}
		return Image{}, fmt.Errorf("open photo: %w", err)
	}
	return image, nil
}

func clonePhoto(selected Photo) Photo {
	clone := selected
	if selected.Metadata.Coordinates != nil {
		coordinates := *selected.Metadata.Coordinates
		clone.Metadata.Coordinates = &coordinates
	}
	if selected.Metadata.EXIF != nil {
		clone.Metadata.EXIF = make(map[string]string, len(selected.Metadata.EXIF))
		for name, value := range selected.Metadata.EXIF {
			clone.Metadata.EXIF[name] = value
		}
	}
	return clone
}
