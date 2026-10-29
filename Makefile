GO ?= go
BUILD_DIR ?= bin
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)

LDFLAGS := -s -w
BUILD_FLAGS := -trimpath -ldflags="$(LDFLAGS)"

.PHONY: all build mcp codex-cli codex-consumer codex-server migrate clean

all: build

build: mcp codex-cli codex-consumer codex-server migrate

mcp:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/codex-mcp ./cmd/mcp

codex-cli:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/codex-cli ./cmd/codex-cli

codex-consumer:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/codex-consumer ./cmd/codex-consumer

codex-server:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/codex-server ./cmd/codex-server

migrate:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/codex-migrate ./cmd/migrate

clean:
	rm -rf $(BUILD_DIR)
