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
const subscriberBufferSize = 16
const catalogCacheTTL = time.Hour

var (
	// ErrNoPhotos reports that the requested selection contains no available photos.
	ErrNoPhotos = errors.New("no photos found")
	// ErrInvalidAlbum reports that an album name is not a local catalog path.
	ErrInvalidAlbum = errors.New("invalid album")
	// ErrPhotoNotFound reports that a requested image is no longer in the photo catalog.
	ErrPhotoNotFound = errors.New("photo not found")
	// ErrPhotoNotInHistory reports that feedback targets a photo not shown recently.
	ErrPhotoNotInHistory = errors.New("photo not found in recent history")
	// ErrInvalidFeedback reports that feedback fields are invalid or inconsistent.
	ErrInvalidFeedback = errors.New("invalid photo feedback")
	// ErrFeedbackRecorderUnavailable reports that feedback cannot be persisted.
	ErrFeedbackRecorderUnavailable = errors.New("feedback recorder unavailable")
)

// Service selects catalog photos and records successful selections in memory.
type Service struct {
	source           Source
	recorder         EventRecorder
	catalogMu        sync.Mutex
	catalogPhotos    []Photo
	catalogExpiresAt time.Time
	catalogCached    bool
	catalogFlight    *catalogFlight
	mu               sync.Mutex
	history          []HistoryEntry
	subscribers      map[uint64]chan HistoryEntry
	nextSubscriberID uint64
}

type catalogFlight struct {
	done   chan struct{}
	photos []Photo
	err    error
}

// NewService creates a photo service backed by source and recorder. If recorder is nil, selections are recorded in memory only.
func NewService(source Source, recorder EventRecorder) *Service {
	return &Service{source: source, recorder: recorder}
}

// Subscribe registers a live selection subscriber. The returned cancellation function is safe to call more than once.
// Subscribers that cannot keep up with the bounded event buffer are closed and removed.
func (s *Service) Subscribe() (<-chan HistoryEntry, func()) {
	s.mu.Lock()
	if s.subscribers == nil {
		s.subscribers = make(map[uint64]chan HistoryEntry)
	}
	id := s.nextSubscriberID
	s.nextSubscriberID++
	events := make(chan HistoryEntry, subscriberBufferSize)
	s.subscribers[id] = events
	s.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			s.mu.Lock()
			if events, ok := s.subscribers[id]; ok {
				delete(s.subscribers, id)
				close(events)
			}
			s.mu.Unlock()
		})
	}
	return events, unsubscribe
}

// Next selects and verifies a photo, avoiding the previous photo when alternatives exist.
func (s *Service) Next(ctx context.Context, album string) (Photo, error) {
	if album != "" && (!fs.ValidPath(album) || path.Clean(album) != album || strings.Contains(album, "\\")) {
		return Photo{}, ErrInvalidAlbum
	}

	photos, err := s.catalog(ctx)
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

	var previous []Photo
	if len(candidates) > 1 && len(s.history) > 0 {
		previousPath := s.history[0].Photo.Path
		withoutPrevious := make([]Photo, 0, len(candidates)-1)
		for _, candidate := range candidates {
			if candidate.Path != previousPath {
				withoutPrevious = append(withoutPrevious, candidate)
			} else {
				previous = append(previous, candidate)
			}
		}
		if len(withoutPrevious) > 0 {
			candidates = withoutPrevious
		}
	}

	var selected Photo
	for len(candidates) > 0 || len(previous) > 0 {
		if len(candidates) == 0 {
			candidates, previous = previous, nil
		}
		index := rand.IntN(len(candidates))
		candidate := candidates[index]
		candidates[index] = candidates[len(candidates)-1]
		candidates = candidates[:len(candidates)-1]

		image, err := s.source.Open(ctx, candidate.Path)
		if err != nil {
			if errors.Is(err, ErrPhotoNotFound) {
				continue
			}
			return Photo{}, fmt.Errorf("verify selected photo: %w", err)
		}
		if err := image.Content.Close(); err != nil {
			return Photo{}, fmt.Errorf("close selected photo: %w", err)
		}
		selected = candidate
		break
	}
	if selected.Path == "" {
		return Photo{}, ErrNoPhotos
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
	s.publish(entry)
	return clonePhoto(selected), nil
}

func (s *Service) catalog(ctx context.Context) ([]Photo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.catalogMu.Lock()
	if s.catalogCached && time.Now().Before(s.catalogExpiresAt) {
		photos := clonePhotos(s.catalogPhotos)
		s.catalogMu.Unlock()
		return photos, nil
	}
	if flight := s.catalogFlight; flight != nil {
		s.catalogMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if flight.err != nil {
				return nil, flight.err
			}
			return clonePhotos(flight.photos), nil
		}
	}

	flight := &catalogFlight{done: make(chan struct{})}
	s.catalogFlight = flight
	s.catalogMu.Unlock()

	photos, err := s.source.List(ctx)
	if err == nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		} else {
			photos = clonePhotos(photos)
		}
	}

	s.catalogMu.Lock()
	if err == nil {
		s.catalogPhotos = photos
		s.catalogExpiresAt = time.Now().Add(catalogCacheTTL)
		s.catalogCached = true
	}
	flight.photos = photos
	flight.err = err
	s.catalogFlight = nil
	close(flight.done)
	s.catalogMu.Unlock()
	if err != nil {
		return nil, err
	}
	return clonePhotos(photos), nil
}

func (s *Service) publish(entry HistoryEntry) {
	for id, events := range s.subscribers {
		select {
		case events <- HistoryEntry{Photo: clonePhoto(entry.Photo), ShownAt: entry.ShownAt}:
		default:
			close(events)
			delete(s.subscribers, id)
		}
	}
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

// RecordFeedback persists feedback for the newest recent selection matching relativePath.
func (s *Service) RecordFeedback(relativePath string, feedback Feedback) error {
	feedback, err := validateFeedback(feedback)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range s.history {
		if entry.Photo.Path != relativePath {
			continue
		}
		if s.recorder == nil {
			return ErrFeedbackRecorderUnavailable
		}
		event := FeedbackEvent{
			Type:    feedback.Type,
			Path:    entry.Photo.Path,
			ShownAt: entry.ShownAt,
			Rating:  feedback.Rating,
			Tag:     feedback.Tag,
		}
		if err := s.recorder.RecordFeedback(event); err != nil {
			return fmt.Errorf("record photo feedback: %w", err)
		}
		return nil
	}
	return ErrPhotoNotInHistory
}

// OpenImage opens a photo only if the cached discovery includes its relative path.
func (s *Service) OpenImage(ctx context.Context, relativePath string) (Image, error) {
	if !fs.ValidPath(relativePath) || path.Clean(relativePath) != relativePath {
		return Image{}, ErrPhotoNotFound
	}

	photos, err := s.catalog(ctx)
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

func clonePhotos(photos []Photo) []Photo {
	clones := make([]Photo, len(photos))
	for index, photo := range photos {
		clones[index] = clonePhoto(photo)
	}
	return clones
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

func validateFeedback(feedback Feedback) (Feedback, error) {
	switch feedback.Type {
	case FeedbackReject:
		if feedback.Rating != 0 || feedback.Tag != "" {
			return Feedback{}, ErrInvalidFeedback
		}
	case FeedbackRating:
		if feedback.Rating < 1 || feedback.Rating > 5 || feedback.Tag != "" {
			return Feedback{}, ErrInvalidFeedback
		}
	case FeedbackFaultyTag:
		feedback.Tag = strings.TrimSpace(feedback.Tag)
		if feedback.Tag == "" || feedback.Rating != 0 {
			return Feedback{}, ErrInvalidFeedback
		}
	default:
		return Feedback{}, ErrInvalidFeedback
	}
	return feedback, nil
}
