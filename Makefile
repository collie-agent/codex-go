GO ?= go
BUILD_DIR ?= bin
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)

LDFLAGS := -s -w
BUILD_FLAGS := -trimpath -ldflags="$(LDFLAGS)"

.PHONY: all build mcp agent-server clean

all: build

build: mcp agent-server

mcp:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/codex-mcp ./cmd/mcp

agent-server:
	mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/agent-server ./cmd/agent-server

clean:
	rm -rf $(BUILD_DIR)
