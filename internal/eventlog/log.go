// Package eventlog appends photo selection and feedback events as JSON Lines.
package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"photo-server/internal/photo"
)

const recoveryChunkSize = 32 * 1024

type Log struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
}

type selectionEvent struct {
	Path     string         `json:"path"`
	Album    string         `json:"album,omitempty"`
	Metadata photo.Metadata `json:"metadata"`
	ShownAt  time.Time      `json:"shown_at"`
}

// Open opens or creates a JSON Lines event log and removes an incomplete trailing record.
func Open(path string) (*Log, error) {
	if path == "" {
		return nil, errors.New("event log path is empty")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	if err := recoverTrailingRecord(file); err != nil {
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("recover event log: %w", err), closeErr)
	}
	return &Log{file: file}, nil
}

// Record appends one successful photo selection as a newline-terminated JSON object.
func (l *Log) Record(entry photo.HistoryEntry) error {
	event := selectionEvent{
		Path:     entry.Photo.Path,
		Album:    entry.Photo.Album,
		Metadata: entry.Photo.Metadata,
		ShownAt:  entry.ShownAt,
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode photo selection event: %w", err)
	}
	encoded = append(encoded, '\n')
	return l.appendEvent(encoded)
}

// RecordFeedback appends one feedback event as a newline-terminated JSON object.
func (l *Log) RecordFeedback(event photo.FeedbackEvent) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode photo feedback event: %w", err)
	}
	encoded = append(encoded, '\n')
	return l.appendEvent(encoded)
}

func (l *Log) appendEvent(encoded []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("event log is closed")
	}

	info, err := l.file.Stat()
	if err != nil {
		return fmt.Errorf("stat event log: %w", err)
	}
	start := info.Size()
	written, writeErr := l.file.Write(encoded)
	if writeErr == nil && written != len(encoded) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return fmt.Errorf("append event: %w", l.rollback(start, writeErr))
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync event log: %w", l.rollback(start, err))
	}
	return nil
}

// Close closes the log. It is safe to call Close more than once.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("close event log: %w", err)
	}
	return nil
}

func (l *Log) rollback(size int64, cause error) error {
	if err := l.file.Truncate(size); err != nil {
		return errors.Join(cause, fmt.Errorf("truncate incomplete event: %w", err))
	}
	if err := l.file.Sync(); err != nil {
		return errors.Join(cause, fmt.Errorf("sync event log rollback: %w", err))
	}
	return cause
}

func recoverTrailingRecord(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat event log: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	var lastByte [1]byte
	if _, err := file.ReadAt(lastByte[:], size-1); err != nil {
		return fmt.Errorf("read event log tail: %w", err)
	}
	if lastByte[0] == '\n' {
		return nil
	}

	buffer := make([]byte, recoveryChunkSize)
	for end := size; end > 0; {
		start := max(int64(0), end-int64(len(buffer)))
		length := int(end - start)
		read, err := file.ReadAt(buffer[:length], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("scan event log tail: %w", err)
		}
		if read != length {
			return fmt.Errorf("scan event log tail: %w", io.ErrUnexpectedEOF)
		}
		for index := read - 1; index >= 0; index-- {
			if buffer[index] == '\n' {
				return truncateAndSync(file, start+int64(index)+1)
			}
		}
		end = start
	}
	return truncateAndSync(file, 0)
}

func truncateAndSync(file *os.File, size int64) error {
	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("truncate incomplete event: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync recovered event log: %w", err)
	}
	return nil
}
