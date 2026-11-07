package filesystem

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"photo-server/internal/photo"
)

func TestCatalogListAndMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "trips", "coast"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "cover.jpg"), exifJPEG())
	writeFixture(t, filepath.Join(root, "trips", "coast", "no-exif.jpg"), []byte{0xff, 0xd8, 0xff, 0xd9})
	writeFixture(t, filepath.Join(root, "notes.txt"), []byte("not a photo"))
	outside := filepath.Join(t.TempDir(), "outside.jpg")
	writeFixture(t, outside, exifJPEG())
	if err := os.Symlink(outside, filepath.Join(root, "outside-link.jpg")); err != nil {
		t.Fatal(err)
	}

	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := catalog.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	photos, err := catalog.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 2 {
		t.Fatalf("List() returned %d photos, want 2: %#v", len(photos), photos)
	}
	if photos[0].Path != "cover.jpg" || photos[0].Album != "" {
		t.Fatalf("root photo = %#v, want cover.jpg without album", photos[0])
	}
	if photos[0].Metadata.CapturedAt != "2020-01-02T03:04:05" {
		t.Fatalf("captured_at = %q, want timezone-less EXIF time", photos[0].Metadata.CapturedAt)
	}
	if photos[0].Metadata.Coordinates == nil {
		t.Fatal("root photo has no GPS coordinates")
	}
	if latitude := photos[0].Metadata.Coordinates.Latitude; math.Abs(latitude-37.8083333333) > 0.000001 {
		t.Fatalf("latitude = %f, want about 37.808333", latitude)
	}
	if longitude := photos[0].Metadata.Coordinates.Longitude; math.Abs(longitude+122.4041666667) > 0.000001 {
		t.Fatalf("longitude = %f, want about -122.404167", longitude)
	}
	if len(photos[0].Metadata.EXIF) == 0 {
		t.Fatal("root photo has no decoded EXIF tags")
	}
	if photos[1].Path != "trips/coast/no-exif.jpg" || photos[1].Album != "trips/coast" {
		t.Fatalf("nested photo = %#v, want nested album path", photos[1])
	}
	if photos[1].Metadata.CapturedAt != "" || photos[1].Metadata.Coordinates != nil || len(photos[1].Metadata.EXIF) != 0 {
		t.Fatalf("photo without EXIF has metadata %#v, want empty metadata", photos[1].Metadata)
	}
}

func TestCatalogOpenAndRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "photo.jpg"), []byte{0xff, 0xd8, 0xff, 0xd9})
	outside := filepath.Join(t.TempDir(), "secret.jpg")
	writeFixture(t, outside, []byte{0xff, 0xd8, 0xff, 0xd9})
	if err := os.Symlink(outside, filepath.Join(root, "escape.jpg")); err != nil {
		t.Fatal(err)
	}
	catalog, err := NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := catalog.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	image, err := catalog.Open(context.Background(), "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if image.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg", image.ContentType)
	}
	if err := image.Content.Close(); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../secret.jpg", "escape.jpg", "missing.jpg", "notes.txt"} {
		t.Run(name, func(t *testing.T) {
			opened, err := catalog.Open(context.Background(), name)
			if !errors.Is(err, photo.ErrPhotoNotFound) {
				if err == nil {
					_ = opened.Content.Close()
				}
				t.Fatalf("Open(%q) error = %v, want ErrPhotoNotFound", name, err)
			}
		})
	}
}

func TestCatalogListCanceledContext(t *testing.T) {
	catalog, err := NewCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := catalog.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalog.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List() error = %v, want context.Canceled", err)
	}
}

func writeFixture(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func exifJPEG() []byte {
	tiff := make([]byte, 160)
	copy(tiff[:2], "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 2)
	putEntry := func(offset int, tag, format uint16, count, value uint32) {
		binary.LittleEndian.PutUint16(tiff[offset:offset+2], tag)
		binary.LittleEndian.PutUint16(tiff[offset+2:offset+4], format)
		binary.LittleEndian.PutUint32(tiff[offset+4:offset+8], count)
		binary.LittleEndian.PutUint32(tiff[offset+8:offset+12], value)
	}
	putEntry(10, 0x0132, 2, 20, 38)
	putEntry(22, 0x8825, 4, 1, 58)
	copy(tiff[38:58], "2020:01:02 03:04:05\x00")
	binary.LittleEndian.PutUint16(tiff[58:60], 4)
	putEntry(60, 1, 2, 2, uint32('N'))
	putEntry(72, 2, 5, 3, 112)
	putEntry(84, 3, 2, 2, uint32('W'))
	putEntry(96, 4, 5, 3, 136)
	putRational := func(offset int, numerator, denominator uint32) {
		binary.LittleEndian.PutUint32(tiff[offset:offset+4], numerator)
		binary.LittleEndian.PutUint32(tiff[offset+4:offset+8], denominator)
	}
	putRational(112, 37, 1)
	putRational(120, 48, 1)
	putRational(128, 30, 1)
	putRational(136, 122, 1)
	putRational(144, 24, 1)
	putRational(152, 15, 1)

	payload := append([]byte("Exif\x00\x00"), tiff...)
	segmentLength := len(payload) + 2
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, byte(segmentLength >> 8), byte(segmentLength)}
	jpeg = append(jpeg, payload...)
	return append(jpeg, 0xff, 0xd9)
}
