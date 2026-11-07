package eventlog_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"photo-server/internal/eventlog"
	"photo-server/internal/photo"
)

type selectionEvent struct {
	Path     string         `json:"path"`
	Album    string         `json:"album,omitempty"`
	Metadata photo.Metadata `json:"metadata"`
	ShownAt  time.Time      `json:"shown_at"`
}

func TestLogAppendAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	first := testEntry("album/first.jpg", time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC))
	second := testEntry("album/second.jpg", time.Date(2026, 11, 7, 15, 1, 0, 0, time.UTC))

	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Record(first); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	log, err = eventlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Record(second); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	events := readEvents(t, path)
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	assertEvent(t, events[0], first)
	assertEvent(t, events[1], second)
}

func TestLogRecoversIncompleteTrailingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	validLine := `{"path":"kept.jpg","metadata":{},"shown_at":"2026-11-07T15:00:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(validLine+`{"path":"partial`), 0o600); err != nil {
		t.Fatal(err)
	}

	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := testEntry("new.jpg", time.Date(2026, 11, 7, 15, 2, 0, 0, time.UTC))
	if err := log.Record(entry); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "partial") {
		t.Fatalf("incomplete trailing data was retained: %s", contents)
	}
	events := readEvents(t, path)
	if len(events) != 2 || events[0].Path != "kept.jpg" || events[1].Path != entry.Photo.Path {
		t.Fatalf("recovered events = %#v, want preserved and appended events", events)
	}
}

func TestLogConcurrentRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	const count = 50
	var workers sync.WaitGroup
	for index := range count {
		workers.Add(1)
		go func() {
			defer workers.Done()
			entry := testEntry(fmt.Sprintf("photo-%d.jpg", index), time.Date(2026, 11, 7, 15, 0, index, 0, time.UTC))
			if err := log.Record(entry); err != nil {
				t.Errorf("record event: %v", err)
			}
		}()
	}
	workers.Wait()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	events := readEvents(t, path)
	if len(events) != count {
		t.Fatalf("event count = %d, want %d", len(events), count)
	}
}

func TestLogOpenFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "events.jsonl")
	if _, err := eventlog.Open(path); err == nil {
		t.Fatal("Open() error = nil, want failure when parent directory does not exist")
	}
}

func TestLogRecordAfterClose(t *testing.T) {
	log, err := eventlog.Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := log.Record(testEntry("photo.jpg", time.Now().UTC())); err == nil {
		t.Fatal("Record() error = nil after Close(), want error")
	}
}

func testEntry(path string, shownAt time.Time) photo.HistoryEntry {
	return photo.HistoryEntry{
		Photo: photo.Photo{
			Path:     path,
			Album:    "album",
			Metadata: photo.Metadata{CapturedAt: "2024-06-18T14:32:00", EXIF: map[string]string{"Make": "Camera"}},
		},
		ShownAt: shownAt,
	}
}

func readEvents(t *testing.T, path string) []selectionEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close event file: %v", err)
		}
	}()

	var events []selectionEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event selectionEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode JSONL event %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func assertEvent(t *testing.T, got selectionEvent, want photo.HistoryEntry) {
	t.Helper()
	if got.Path != want.Photo.Path || got.Album != want.Photo.Album || !got.ShownAt.Equal(want.ShownAt) {
		t.Fatalf("event = %#v, want path %q, album %q, shown_at %v", got, want.Photo.Path, want.Photo.Album, want.ShownAt)
	}
	if got.Metadata.CapturedAt != want.Photo.Metadata.CapturedAt || got.Metadata.EXIF["Make"] != want.Photo.Metadata.EXIF["Make"] {
		t.Fatalf("event metadata = %#v, want %#v", got.Metadata, want.Photo.Metadata)
	}
}
