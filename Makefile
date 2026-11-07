GO ?= go
CONTAINER_ENGINE ?= container
IMAGE_NAME ?= photo-server
BUILD_DIR ?= build

.PHONY: build build-macos-arm64 build-linux-arm64 build-linux-amd64 container-images container-arm64 container-amd64 clean

build: build-macos-arm64 build-linux-arm64 build-linux-amd64

build-macos-arm64:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o $(BUILD_DIR)/photo-server-darwin-arm64 ./cmd/photo-server

build-linux-arm64:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -o $(BUILD_DIR)/photo-server-linux-arm64 ./cmd/photo-server

build-linux-amd64:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o $(BUILD_DIR)/photo-server-linux-amd64 ./cmd/photo-server

container-images: container-arm64 container-amd64

container-arm64:
	$(CONTAINER_ENGINE) build --platform linux/arm64 -t $(IMAGE_NAME):linux-arm64 .

container-amd64:
	$(CONTAINER_ENGINE) build --platform linux/amd64 -t $(IMAGE_NAME):linux-amd64 .

clean:
	rm -rf $(BUILD_DIR)