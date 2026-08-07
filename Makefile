GO          ?= go
CLI_PLUGIN  = docker-dvol
BIN         = bin/dvol-cli-plugin bin/$(CLI_PLUGIN)

.PHONY: all build test lint e2e acceptance acceptance-mongo cli-plugin-build cli-plugin-install clean

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

## acceptance: run all acceptance suites through the binary and docker dvol
## Build first as the normal user; only the acceptance run needs privileges.
acceptance:
	@test -x bin/dvol-cli-plugin || { echo "run 'make build' before 'sudo make acceptance'" >&2; exit 1; }
	@mkdir -p "$(HOME)/.docker/cli-plugins"
	@rm -f "$(HOME)/.docker/cli-plugins/docker-dvol-linux"
	@cp bin/dvol-cli-plugin "$(HOME)/.docker/cli-plugins/docker-dvol"
	@chmod +x "$(HOME)/.docker/cli-plugins/docker-dvol"
	@echo "Acceptance CLI plugin: $(HOME)/.docker/cli-plugins/docker-dvol"
	./test/acceptance_e2e.sh
	./test/acceptance_mongo_e2e.sh
	DVOL_MODE=docker ./test/acceptance_e2e.sh
	DVOL_MODE=docker ./test/acceptance_mongo_e2e.sh

## acceptance-mongo: run the MongoDB matrix through both client paths
acceptance-mongo:
	@test -x bin/dvol-cli-plugin || { echo "run 'make build' before 'sudo make acceptance-mongo'" >&2; exit 1; }
	@mkdir -p "$(HOME)/.docker/cli-plugins"
	@rm -f "$(HOME)/.docker/cli-plugins/docker-dvol-linux"
	@cp bin/dvol-cli-plugin "$(HOME)/.docker/cli-plugins/docker-dvol"
	@chmod +x "$(HOME)/.docker/cli-plugins/docker-dvol"
	./test/acceptance_mongo_e2e.sh
	DVOL_MODE=docker ./test/acceptance_mongo_e2e.sh

## lint: go vet
lint:
	$(GO) vet ./...

## cli-plugin-build: build the Docker CLI plugin only
cli-plugin-build: build

## cli-plugin-install: install `docker dvol` for the current user
cli-plugin-install: build
	mkdir -p $(HOME)/.docker/cli-plugins
	rm -f $(HOME)/.docker/cli-plugins/docker-dvol-linux
	cp bin/dvol-cli-plugin $(HOME)/.docker/cli-plugins/docker-dvol
	chmod +x $(HOME)/.docker/cli-plugins/docker-dvol
	@echo "installed: ~/.docker/cli-plugins/docker-dvol (try: docker dvol --help)"

## clean: remove build artifacts
clean:
	rm -rf bin
