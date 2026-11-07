# Photo Server

Photo Server is a small HTTP service for slideshow clients. It discovers supported images on disk, samples photos from the full collection or a named album, serves the image bytes, and keeps the ten most recent successful selections in memory.

## Getting started

Requires Go 1.27 or later. From the project root, run:

```sh
mkdir -p photos
go run ./cmd/photo-server
```

The service listens on `127.0.0.1:8080` and reads images from `./photos` by default. Put images directly in that directory or in subdirectories; each containing subdirectory is an album. Set `PHOTO_DIR` to use another directory and `HTTP_ADDR` to change the listen address:

```sh
PHOTO_DIR=/path/to/photos HTTP_ADDR=0.0.0.0:8080 go run ./cmd/photo-server
```

The default listen address is limited to the local machine. Use a reachable interface address when slideshow clients run on other devices, and keep the service on a trusted network because it has no authentication.

To try the service with the included AI-generated demo albums, run:

```sh
PHOTO_DIR=./demo go run ./cmd/photo-server
```

Use `landscapes` or `imagination` as the album name. See [demo/README.md](demo/README.md) for image sources and licenses.

## API

### `GET /api/photos/next`

Returns a randomly selected photo from all albums. An optional `album` query parameter selects one exact album path, including nested paths such as `trips/coast`:

```sh
curl 'http://127.0.0.1:8080/api/photos/next?album=trips%2Fcoast'
```

Example response:

```json
{
  "path": "trips/coast/photo.jpg",
  "album": "trips/coast",
  "metadata": {
    "captured_at": "2024-06-18T14:32:00",
    "coordinates": {
      "latitude": 37.8083,
      "longitude": -122.4042
    },
    "exif": {
      "Make": "Camera manufacturer"
    }
  },
  "image_url": "/api/photos/dHJpcHMvY29hc3QvcGhvdG8uanBn/image"
}
```

The `path` is relative to `PHOTO_DIR`. The `image_url` is a stable URL for that path while the file remains in the catalog. A photo is not immediately repeated when another eligible photo is available. If no eligible photo exists, the endpoint responds with `404`.

### `GET /api/photos/{id}/image`

Streams the selected image bytes. The `id` is opaque and is supplied in `image_url`; clients should not construct it. The endpoint returns `404` if the file has been removed or is no longer a supported catalog photo.

### `GET /api/photos/history`

Returns up to ten successful selections, newest first. Each history entry includes the relative disk path, album (when applicable), available metadata, image URL, and `shown_at`: the UTC RFC 3339 timestamp recorded for that selection. `shown_at` describes when the service selected the photo; it is distinct from EXIF `captured_at`, which describes when the image was taken and may have no timezone. History is held in memory and resets when the process restarts. Delivering an image does not add another history entry.

### Feedback endpoints

Send feedback using the opaque ID from a photo's `image_url` or a history entry's `image_url`:

| Endpoint | Request body | Description |
| --- | --- | --- |
| `POST /api/photos/{id}/reject` | Empty | Rejects the shown photo. |
| `POST /api/photos/{id}/rating` | `{"rating": 1}` through `{"rating": 5}` | Rates the photo from one to five. |
| `POST /api/photos/{id}/faulty-tag` | `{"tag": "incorrect location"}` | Reports a faulty EXIF tag by name. |

The ID must refer to a photo in the ten most recent successful selections. If the same photo was selected more than once, feedback is associated with its newest selection. A successful request returns `204 No Content`; malformed feedback returns `400`, and an unknown or no-longer-recent ID returns `404`. The service returns `503` if feedback cannot be durably recorded.

Each successful selection is appended to the event log as one newline-delimited JSON object containing `path`, optional `album`, `metadata`, and `shown_at`. Feedback is appended to the same log with an event `type`, `path`, and the matching selection's `shown_at`; rating events also include `rating`, and faulty-tag events include `tag`. For example:

```json
{"type":"reject","path":"trips/coast/photo.jpg","shown_at":"2026-11-07T15:02:00Z"}
{"type":"rating","path":"trips/coast/photo.jpg","shown_at":"2026-11-07T15:02:00Z","rating":4}
{"type":"faulty_tag","path":"trips/coast/photo.jpg","shown_at":"2026-11-07T15:02:00Z","tag":"incorrect location"}
```

The append-only log survives restarts; it is separate from the bounded in-memory history, and only one server process should write to a given log file. On startup, an incomplete trailing line is truncated so later events remain valid JSON Lines. If a selection or feedback event cannot be saved, the request fails and the corresponding operation is not reported as successful.

## Photos and metadata

The service recognizes `.avif`, `.bmp`, `.gif`, `.jpeg`, `.jpg`, `.png`, `.tif`, `.tiff`, and `.webp` files. It rescans the directory tree when selecting or serving a photo, so additions and removals become visible without restarting the process. Symbolic links are not cataloged, and image opens are confined to the configured photo root.

EXIF tags are returned when present and parseable. Capture time and GPS coordinates are optional; malformed or missing EXIF does not prevent an image from being served. EXIF timestamps usually do not contain a timezone, so `captured_at` is returned without an offset and must not be interpreted as UTC.

## Configuration

| Environment variable | Default | Description |
| --- | --- | --- |
| `PHOTO_DIR` | `./photos` | Root directory containing image files and album directories. |
| `EVENT_LOG` | `./events.jsonl` | File receiving append-only JSON Lines selection and feedback events. Its parent directory must exist. |
| `HTTP_ADDR` | `127.0.0.1:8080` | TCP address for the HTTP server. |

## Tests

Run the unit and HTTP tests, including the race detector, with:

```sh
go test ./...
go test -race ./...
```

GitHub Actions runs these checks on pull requests and pushes to `main`: dependency verification and tidy checks, formatting, build, race-enabled shuffled tests, `go vet`, `golangci-lint`, and `govulncheck`.