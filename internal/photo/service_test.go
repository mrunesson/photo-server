package photo

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type memorySource struct {
	photos  []Photo
	openErr error
}

type memoryRecorder struct {
	entries []HistoryEntry
	err     error
}

func (r *memoryRecorder) Record(entry HistoryEntry) error {
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, entry)
	return nil
}

func (s *memorySource) List(context.Context) ([]Photo, error) {
	return append([]Photo(nil), s.photos...), nil
}

func (s *memorySource) Open(_ context.Context, relativePath string) (Image, error) {
	if s.openErr != nil {
		return Image{}, s.openErr
	}
	for _, candidate := range s.photos {
		if candidate.Path == relativePath {
			return Image{Content: nopReadSeekCloser{}}, nil
		}
	}
	return Image{}, ErrPhotoNotFound
}

type nopReadSeekCloser struct{}

func (nopReadSeekCloser) Read([]byte) (int, error)       { return 0, io.EOF }
func (nopReadSeekCloser) Seek(int64, int) (int64, error) { return 0, nil }
func (nopReadSeekCloser) Close() error                   { return nil }

func TestServiceNext(t *testing.T) {
	tests := []struct {
		name     string
		photos   []Photo
		album    string
		wantErr  error
		wantPath string
	}{
		{
			name: "selects from all albums",
			photos: []Photo{
				{Path: "a.jpg", Album: "a"},
				{Path: "b.jpg", Album: "b"},
			},
		},
		{
			name: "selects requested album",
			photos: []Photo{
				{Path: "a.jpg", Album: "a"},
				{Path: "b.jpg", Album: "b"},
			},
			album:    "b",
			wantPath: "b.jpg",
		},
		{
			name:    "empty collection",
			wantErr: ErrNoPhotos,
		},
		{
			name:    "invalid album path",
			photos:  []Photo{{Path: "a.jpg"}},
			album:   "../private",
			wantErr: ErrInvalidAlbum,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewService(&memorySource{photos: tt.photos}, nil)
			got, err := service.Next(context.Background(), tt.album)
			if !errors.Is(err, tt.wantErr) {
				if err != nil || tt.wantErr != nil {
					t.Fatalf("Next() error = %v, want %v", err, tt.wantErr)
				}
			}
			if err != nil {
				return
			}
			if tt.wantPath != "" && got.Path != tt.wantPath {
				t.Fatalf("Next() path = %q, want %q", got.Path, tt.wantPath)
			}
			if tt.album == "" && got.Path != "a.jpg" && got.Path != "b.jpg" {
				t.Fatalf("Next() selected unexpected path %q", got.Path)
			}
		})
	}
}

func TestServiceNextAvoidsImmediateRepeatAndAllowsSinglePhoto(t *testing.T) {
	service := NewService(&memorySource{photos: []Photo{{Path: "a.jpg"}, {Path: "b.jpg"}}}, nil)
	previous, err := service.Next(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	next, err := service.Next(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Path == previous.Path {
		t.Fatalf("consecutive selections both returned %q", next.Path)
	}

	single := NewService(&memorySource{photos: []Photo{{Path: "only.jpg"}}}, nil)
	for range 2 {
		got, err := single.Next(context.Background(), "")
		if err != nil || got.Path != "only.jpg" {
			t.Fatalf("Next() = (%q, %v), want only.jpg", got.Path, err)
		}
	}
}

func TestServiceFailedSelectionIsNotRecorded(t *testing.T) {
	source := &memorySource{photos: []Photo{{Path: "a.jpg"}}, openErr: errors.New("removed")}
	recorder := &memoryRecorder{}
	service := NewService(source, recorder)
	if _, err := service.Next(context.Background(), ""); err == nil {
		t.Fatal("Next() error = nil, want failure opening selected photo")
	}
	if history := service.History(); len(history) != 0 {
		t.Fatalf("History() = %v, want empty", history)
	}
	if len(recorder.entries) != 0 {
		t.Fatalf("recorded events = %d, want 0 after failed photo verification", len(recorder.entries))
	}
}

func TestServiceRecordsSuccessfulSelectionsAndRejectsRecorderFailure(t *testing.T) {
	photos := []Photo{{Path: "a.jpg", Metadata: Metadata{CapturedAt: "2024-06-18T14:32:00"}}}
	recorder := &memoryRecorder{}
	service := NewService(&memorySource{photos: photos}, recorder)
	selected, err := service.Next(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	history := service.History()
	if len(recorder.entries) != 1 || len(history) != 1 {
		t.Fatalf("recorded events = %d, history entries = %d, want one each", len(recorder.entries), len(history))
	}
	if recorder.entries[0].Photo.Path != selected.Path || history[0].Photo.Path != selected.Path ||
		!recorder.entries[0].ShownAt.Equal(history[0].ShownAt) || recorder.entries[0].ShownAt.IsZero() {
		t.Fatalf("recorded event = %#v, history = %#v, selected = %#v", recorder.entries[0], history[0], selected)
	}

	recordErr := errors.New("disk full")
	failedRecorder := &memoryRecorder{err: recordErr}
	failedService := NewService(&memorySource{photos: photos}, failedRecorder)
	if _, err := failedService.Next(context.Background(), ""); !errors.Is(err, recordErr) {
		t.Fatalf("Next() error = %v, want wrapped event log error", err)
	}
	if got := len(failedService.History()); got != 0 {
		t.Fatalf("history length after failed event write = %d, want 0", got)
	}
}

func TestServiceHistoryNewestFirstLimitAndCopies(t *testing.T) {
	photos := make([]Photo, 12)
	for index := range photos {
		photos[index] = Photo{Path: strings.Repeat("x", index+1), Metadata: Metadata{EXIF: map[string]string{"Make": "camera"}}}
	}
	service := NewService(&memorySource{photos: photos}, nil)
	selectedPaths := make([]string, 0, len(photos))
	for range 12 {
		selected, err := service.Next(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		selectedPaths = append(selectedPaths, selected.Path)
		if newest := service.History()[0]; newest.Photo.Path != selected.Path {
			t.Fatalf("newest history path = %q, want %q", newest.Photo.Path, selected.Path)
		}
	}
	history := service.History()
	if len(history) != historyLimit {
		t.Fatalf("History() length = %d, want %d", len(history), historyLimit)
	}
	if history[0].Photo.Path == history[1].Photo.Path {
		t.Fatalf("newest two history entries repeat %q", history[0].Photo.Path)
	}
	for index, selected := range history {
		want := selectedPaths[len(selectedPaths)-1-index]
		if selected.Photo.Path != want {
			t.Fatalf("history[%d].Photo.Path = %q, want %q", index, selected.Photo.Path, want)
		}
	}
	history[0].Photo.Metadata.EXIF["Make"] = "changed"
	if got := service.History()[0].Photo.Metadata.EXIF["Make"]; got != "camera" {
		t.Fatalf("mutating returned history changed service state: %q", got)
	}
}

func TestServiceHistoryTimestamps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := NewService(&memorySource{photos: []Photo{{Path: "only.jpg"}}}, nil)
		selectedAt := make([]time.Time, 0, 3)
		for range 3 {
			before := time.Now().UTC()
			if _, err := service.Next(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			after := time.Now().UTC()
			entry := service.History()[0]
			if entry.Photo.Path != "only.jpg" {
				t.Fatalf("history path = %q, want only.jpg", entry.Photo.Path)
			}
			if entry.ShownAt.IsZero() || entry.ShownAt.Location() != time.UTC {
				t.Fatalf("shown_at = %v, want non-zero UTC timestamp", entry.ShownAt)
			}
			if entry.ShownAt.Before(before) || entry.ShownAt.After(after) {
				t.Fatalf("shown_at = %v, want timestamp between %v and %v", entry.ShownAt, before, after)
			}
			selectedAt = append(selectedAt, entry.ShownAt)
			synctest.Sleep(time.Second)
		}

		history := service.History()
		for index, entry := range history {
			want := selectedAt[len(selectedAt)-1-index]
			if !entry.ShownAt.Equal(want) {
				t.Errorf("history[%d].ShownAt = %v, want %v", index, entry.ShownAt, want)
			}
			if index > 0 && !entry.ShownAt.Before(history[index-1].ShownAt) {
				t.Errorf("history timestamps are not newest first: %v then %v", history[index-1].ShownAt, entry.ShownAt)
			}
		}
	})
}

func TestServiceConcurrentAccess(t *testing.T) {
	photos := []Photo{{Path: "a.jpg"}, {Path: "b.jpg"}, {Path: "c.jpg"}}
	service := NewService(&memorySource{photos: photos}, nil)
	var workers sync.WaitGroup
	for range 50 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := service.Next(context.Background(), ""); err != nil {
				t.Errorf("Next() error = %v", err)
			}
			_ = service.History()
		}()
	}
	workers.Wait()
	if got := len(service.History()); got != historyLimit {
		t.Fatalf("History() length = %d, want %d", got, historyLimit)
	}
}
