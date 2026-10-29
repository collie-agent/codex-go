GO ?= go
BUILD_DIR ?= bin
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)

LDFLAGS := -s -w
BUILD_FLAGS := -trimpath -ldflags="$(LDFLAGS)"

.PHONY: all build mcp codex-cli migrate clean

all: build

build: mcp codex-cli migrate

mcp:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/codex-mcp ./cmd/mcp

codex-cli:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/codex-cli ./cmd/codex-cli

migrate:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/codex-migrate ./cmd/migrate

clean:
	rm -rf $(BUILD_DIR)
