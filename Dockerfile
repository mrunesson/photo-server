FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG TARGETOS
ARG TARGETARCH
RUN mkdir -p /out/rootfs/data /out/rootfs/photos \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
       go build -trimpath -ldflags="-s -w" -o /out/rootfs/photo-server ./cmd/photo-server

FROM scratch

COPY --from=build /out/rootfs/ /

ENV PHOTO_DIR=/photos \
    EVENT_LOG=/data/events.jsonl \
    HTTP_ADDR=:8080

EXPOSE 8080
ENTRYPOINT ["/photo-server"]