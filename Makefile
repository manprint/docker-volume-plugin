GO      ?= go
PREFIX  ?= dvol
ROOTFS_IMG = dvol/rootfs:latest
PLUGIN_DIR  = plugin
PLUGIN_ROOTFS = $(PLUGIN_DIR)/rootfs
BIN      = bin/dvol-plugin bin/dvol-ctl

.PHONY: all build test lint dev clean

all: build

## build: compile host binaries into bin/
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/ ./cmd/...

## test: run unit tests
test:
	$(GO) test ./...

## lint: go vet
lint:
	$(GO) vet ./...

## plugin-build: build the plugin rootfs image (needs `make build`)
plugin-build: build
	docker build -t $(ROOTFS_IMG) -f $(PLUGIN_DIR)/Dockerfile .
	rm -rf $(PLUGIN_ROOTFS)
	mkdir -p $(PLUGIN_ROOTFS)
	id=$$(docker create $(ROOTFS_IMG)); \
	docker export $$id | tar -x -C $(PLUGIN_ROOTFS); \
	docker rm $$id

## plugin-create: pack the rootfs into a docker plugin
plugin-create: plugin-build
	docker plugin rm $(PREFIX):latest 2>/dev/null || true
	docker plugin create $(PREFIX) $(PLUGIN_DIR)

## plugin-install: install the plugin (grants all permissions)
plugin-install: plugin-create
	docker plugin install --grant-all-permissions $(PREFIX)

## plugin-enable: enable a previously installed plugin
plugin-enable:
	docker plugin enable $(PREFIX)

## plugin-remove: disable + remove the plugin
plugin-remove:
	docker plugin disable $(PREFIX) 2>/dev/null || true
	docker plugin rm $(PREFIX) 2>/dev/null || true

## dev: run the driver locally (no docker plugin), data under ./dev-dvol
dev: build
	./bin/dvol-plugin -root ./dev-dvol

## DEV_ROOT=/path make dev-root: run driver with a custom data root
dev-root: build
	./bin/dvol-plugin -root $(DEV_ROOT)

## clean: remove build artifacts
clean:
	rm -rf bin $(PLUGIN_ROOTFS) repro.rootfs dev-dvol