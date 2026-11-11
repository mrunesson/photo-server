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

When the listener is bound to a reachable network interface, Photo Server advertises a DNS-SD service named `Photo Server` as `_photo-server._tcp.local.`. The announcement uses the actual listening port, including when `HTTP_ADDR` requests an OS-assigned port. A wildcard listener advertises active, multicast-capable non-loopback interfaces; a listener bound to one address advertises only that address. Loopback-only listeners are not announced. The default `127.0.0.1:8080` listener therefore remains local-only; set `HTTP_ADDR` to a reachable interface address or `0.0.0.0:8080` for LAN clients.

Before announcing, the server sends one mDNS probe per interface and only advertises on interfaces where multicast works. If no interface works, startup logs `zeroconf discovery unavailable` with the per-interface error and the HTTP server keeps running. On macOS, `sendmsg: no route to host` for `224.0.0.251` or `ff02::fb` means the process is denied Local Network access: the permission belongs to the application that launched the binary (Terminal, an IDE, a launchd job), so enable that application under System Settings > Privacy & Security > Local Network and restart the server. Verify the announcement with `dns-sd -B _photo-server._tcp local.`.

## Building and containers

Run `make build` to create binaries in `build/` for macOS arm64, Linux arm64, and Linux amd64. Individual targets are available as `make build-macos-arm64`, `make build-linux-arm64`, and `make build-linux-amd64`.

Build architecture-specific Linux container images with `make container-images`. The default image names are `photo-server:linux-arm64` and `photo-server:linux-amd64`; set `IMAGE_NAME` to use another image name. The Makefile uses Apple's `container` CLI by default. To use Docker instead, run `make container-images CONTAINER_ENGINE=docker`.

The container listens on port `8080`, reads photos from `/photos`, and writes its event log to `/data/events.jsonl`. Mount the photo directory read-only and provide a persistent writable volume for `/data`:

```sh
docker run --rm -p 8080:8080 \
  -v /path/to/photos:/photos:ro \
  -v photo-server-events:/data \
  photo-server:linux-arm64
```

Use the `linux-amd64` tag for x86-64 hosts. The container defaults do not change the host defaults; set `PHOTO_DIR`, `EVENT_LOG`, or `HTTP_ADDR` to override them.

Container networking may block or isolate mDNS multicast even when the HTTP port is published. DNS-SD discovery is best-effort; clients may need to connect directly to the host address when multicast is unavailable.

To try the service with the included AI-generated demo albums, run:

```sh
PHOTO_DIR=./demo go run ./cmd/photo-server
```

Use `landscapes` or `imagination` as the album name. See [demo/README.md](demo/README.md) for image sources and licenses.

## API

The interactive OpenAPI UI is available at [`/docs/`](http://127.0.0.1:8080/docs/) when the service is running, and the OpenAPI 3 specification is available at [`/docs/openapi.json`](http://127.0.0.1:8080/docs/openapi.json). The UI and its assets are served locally. The canonical specification source is [`internal/httpapi/openapi.json`](internal/httpapi/openapi.json); update that document alongside HTTP handler changes so it stays aligned with the API behavior.

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

### `GET /api/photos/events`

Opens a live Server-Sent Events stream. Connect before requesting the next photo to receive every successful selection made through `GET /api/photos/next`, including selections made by other clients:

```sh
curl -N 'http://127.0.0.1:8080/api/photos/events'
```

Each selection is sent as an event named `photo`; its `data` is a JSON photo response with the UTC `shown_at` timestamp:

```text
event: photo
data: {"path":"trips/coast/photo.jpg","album":"trips/coast","metadata":{},"image_url":"/api/photos/dHJpcHMvY29hc3QvcGhvdG8uanBn/image","shown_at":"2026-11-07T15:02:00Z"}
```

The stream is live-only and does not replay events on reconnect; reconnect to receive future selections. A client that cannot keep up with the bounded event buffer is disconnected and should reconnect. Initial and periodic comment frames keep idle connections open. `HEAD` returns headers without opening a stream. Failed or unrecorded selections do not generate events.

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

The service recognizes `.avif`, `.bmp`, `.gif`, `.jpeg`, `.jpg`, `.png`, `.tif`, `.tiff`, and `.webp` files. It caches the discovered catalog for one hour, so additions become visible on the next refresh; removed files are rejected immediately when opened. Symbolic links are not cataloged, and image opens are confined to the configured photo root.

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