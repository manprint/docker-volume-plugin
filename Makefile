GO          ?= go
CLI_PLUGIN  = docker-dvol-linux
BIN         = bin/dvol-cli-plugin bin/$(CLI_PLUGIN)

.PHONY: all build test lint e2e acceptance cli-plugin-build cli-plugin-install clean

all: build

## build: compile the Docker CLI plugin into bin/
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/dvol-cli-plugin ./cmd/dvol-cli-plugin
	cp bin/dvol-cli-plugin bin/$(CLI_PLUGIN)

## test: run unit tests
test:
	$(GO) test ./...

## e2e: run the backup/restore smoke test
e2e: build
	./test/e2e.sh

## acceptance: run the full PostgreSQL and Odoo Docker acceptance scenarios
## Build first as the normal user; only the acceptance run needs privileges.
acceptance:
	@test -x bin/dvol-cli-plugin || { echo "run 'make build' before 'sudo make acceptance'" >&2; exit 1; }
	./test/acceptance_e2e.sh

## lint: go vet
lint:
	$(GO) vet ./...

## cli-plugin-build: build the Docker CLI plugin only
cli-plugin-build: build

## cli-plugin-install: install `docker dvol` for the current user
cli-plugin-install: build
	mkdir -p $(HOME)/.docker/cli-plugins
	cp bin/dvol-cli-plugin $(HOME)/.docker/cli-plugins/docker-dvol
	cp bin/dvol-cli-plugin $(HOME)/.docker/cli-plugins/docker-dvol-linux
	chmod +x $(HOME)/.docker/cli-plugins/docker-dvol $(HOME)/.docker/cli-plugins/docker-dvol-linux
	@echo "installed: ~/.docker/cli-plugins/docker-dvol (try: docker dvol --help)"

## clean: remove build artifacts
clean:
	rm -rf bin
