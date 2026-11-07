// Package filesystem discovers and safely opens photos stored beneath a configured root.
package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rwcarlsen/goexif/exif"

	"photo-server/internal/photo"
)

var supportedExtensions = []string{".avif", ".bmp", ".gif", ".jpeg", ".jpg", ".png", ".tif", ".tiff", ".webp"}

// Catalog discovers supported image files and confines opens to a fixed filesystem root.
type Catalog struct {
	rootPath string
	root     *os.Root
}

// NewCatalog opens root and resolves it to a stable directory before serving requests.
func NewCatalog(root string) (*Catalog, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve photo root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve photo root symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat photo root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("photo root %q is not a directory", root)
	}
	rootHandle, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("open photo root: %w", err)
	}
	return &Catalog{rootPath: resolved, root: rootHandle}, nil
}

// Close releases the directory handle held for root-confined image access.
func (c *Catalog) Close() error {
	if err := c.root.Close(); err != nil {
		return fmt.Errorf("close photo root: %w", err)
	}
	return nil
}

// List walks the photo root without following symbolic links and extracts available metadata.
func (c *Catalog) List(ctx context.Context) ([]photo.Photo, error) {
	photos := make([]photo.Photo, 0)
	err := filepath.WalkDir(c.rootPath, func(current string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("walk photo catalog: %w", walkErr)
		}
		if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || !isImageFile(entry.Name()) {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("inspect photo %q: %w", current, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(c.rootPath, current)
		if err != nil {
			return fmt.Errorf("make photo path relative: %w", err)
		}
		relative = filepath.ToSlash(relative)
		album := path.Dir(relative)
		if album == "." {
			album = ""
		}
		photos = append(photos, photo.Photo{
			Path:     relative,
			Album:    album,
			Metadata: c.readMetadata(ctx, relative),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover photos: %w", err)
	}
	slices.SortFunc(photos, func(left, right photo.Photo) int {
		return strings.Compare(left.Path, right.Path)
	})
	return photos, nil
}

// Open returns a seekable photo file while preventing access outside the catalog root.
func (c *Catalog) Open(ctx context.Context, relativePath string) (photo.Image, error) {
	if err := ctx.Err(); err != nil {
		return photo.Image{}, err
	}
	if !fs.ValidPath(relativePath) || path.Clean(relativePath) != relativePath || !isImageFile(relativePath) {
		return photo.Image{}, photo.ErrPhotoNotFound
	}
	if err := c.rejectSymlinkComponents(relativePath); err != nil {
		return photo.Image{}, err
	}

	file, err := c.root.Open(filepath.FromSlash(relativePath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return photo.Image{}, photo.ErrPhotoNotFound
		}
		return photo.Image{}, fmt.Errorf("open photo %q: %w", relativePath, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return photo.Image{}, fmt.Errorf("stat photo %q: %w", relativePath, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return photo.Image{}, photo.ErrPhotoNotFound
	}
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(relativePath)))
	if contentType == "" {
		var sample [512]byte
		n, readErr := file.Read(sample[:])
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			_ = file.Close()
			return photo.Image{}, fmt.Errorf("read photo header %q: %w", relativePath, readErr)
		}
		contentType = http.DetectContentType(sample[:n])
		if _, err := file.Seek(0, 0); err != nil {
			_ = file.Close()
			return photo.Image{}, fmt.Errorf("rewind photo %q: %w", relativePath, err)
		}
	}
	return photo.Image{Content: file, ContentType: contentType, ModTime: info.ModTime()}, nil
}

func (c *Catalog) rejectSymlinkComponents(relativePath string) error {
	components := strings.Split(relativePath, "/")
	for index := range components {
		component := strings.Join(components[:index+1], "/")
		info, err := c.root.Lstat(filepath.FromSlash(component))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return photo.ErrPhotoNotFound
			}
			return fmt.Errorf("inspect photo path: %w", err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || (index < len(components)-1 && !info.IsDir()) {
			return photo.ErrPhotoNotFound
		}
	}
	return nil
}

func (c *Catalog) readMetadata(ctx context.Context, relativePath string) photo.Metadata {
	if ctx.Err() != nil {
		return photo.Metadata{}
	}
	file, err := c.root.Open(filepath.FromSlash(relativePath))
	if err != nil {
		return photo.Metadata{}
	}
	defer file.Close()

	decoded, decodeErr := exif.Decode(file)
	if decoded == nil || (decodeErr != nil && exif.IsCriticalError(decodeErr)) {
		return photo.Metadata{}
	}
	metadata := photo.Metadata{}
	if encoded, err := decoded.MarshalJSON(); err == nil {
		var tags map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &tags); err == nil {
			metadata.EXIF = make(map[string]string, len(tags))
			for name, raw := range tags {
				var value string
				if err := json.Unmarshal(raw, &value); err == nil {
					metadata.EXIF[name] = value
				} else {
					metadata.EXIF[name] = string(raw)
				}
			}
		}
	}
	if len(metadata.EXIF) == 0 {
		metadata.EXIF = nil
	}
	if captured, err := decoded.DateTime(); err == nil {
		value := captured.Format("2006-01-02T15:04:05")
		metadata.CapturedAt = value
	}
	if latitude, longitude, err := decoded.LatLong(); err == nil && latitude >= -90 && latitude <= 90 && longitude >= -180 && longitude <= 180 {
		metadata.Coordinates = &photo.Coordinates{Latitude: latitude, Longitude: longitude}
	}
	return metadata
}

func isImageFile(name string) bool {
	extension := strings.ToLower(filepath.Ext(name))
	return slices.Contains(supportedExtensions, extension)
}
