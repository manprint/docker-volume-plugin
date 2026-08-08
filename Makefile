GO          ?= go
CLI_PLUGIN  = docker-dvol
BIN         = bin/dvol-cli-plugin bin/$(CLI_PLUGIN)

.PHONY: all build test test-race fuzz lint check e2e acceptance acceptance-defects acceptance-mongo acceptance-stream cli-plugin-build cli-plugin-install clean

all: build

## build: compile the Docker CLI plugin into bin/
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/dvol-cli-plugin ./cmd/dvol-cli-plugin
	cp bin/dvol-cli-plugin bin/$(CLI_PLUGIN)

## test: run unit tests
test:
	$(GO) test ./...

## test-race: run unit tests under the race detector (concurrency suite)
test-race:
	$(GO) test -race ./...

## fuzz: fuzz the extraction path (FUZZTIME=30s by default)
FUZZTIME ?= 30s
fuzz:
	$(GO) test ./pkg/dvol/ -run '^$$' -fuzz FuzzExtractTarStream -fuzztime $(FUZZTIME)

## check: the quality gate every change must pass before acceptance
check: lint test test-race build

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
	./test/acceptance_defects_e2e.sh
	./test/acceptance_e2e.sh
	./test/acceptance_mongo_e2e.sh
	DVOL_MODE=docker ./test/acceptance_defects_e2e.sh
	DVOL_MODE=docker ./test/acceptance_e2e.sh
	DVOL_MODE=docker ./test/acceptance_mongo_e2e.sh
	./test/acceptance_stream_e2e.sh

## acceptance-defects: regression scenarios for the defects fixed after the review
acceptance-defects:
	@test -x bin/dvol-cli-plugin || { echo "run 'make build' before 'sudo make acceptance-defects'" >&2; exit 1; }
	@mkdir -p "$(HOME)/.docker/cli-plugins"
	@cp bin/dvol-cli-plugin "$(HOME)/.docker/cli-plugins/docker-dvol"
	@chmod +x "$(HOME)/.docker/cli-plugins/docker-dvol"
	./test/acceptance_defects_e2e.sh
	DVOL_MODE=docker ./test/acceptance_defects_e2e.sh

## acceptance-mongo: run the MongoDB matrix through both client paths
acceptance-mongo:
	@test -x bin/dvol-cli-plugin || { echo "run 'make build' before 'sudo make acceptance-mongo'" >&2; exit 1; }
	@mkdir -p "$(HOME)/.docker/cli-plugins"
	@rm -f "$(HOME)/.docker/cli-plugins/docker-dvol-linux"
	@cp bin/dvol-cli-plugin "$(HOME)/.docker/cli-plugins/docker-dvol"
	@chmod +x "$(HOME)/.docker/cli-plugins/docker-dvol"
	./test/acceptance_mongo_e2e.sh
	DVOL_MODE=docker ./test/acceptance_mongo_e2e.sh

## acceptance-stream: test stdin/stdout through docker dvol and a Docker rclone remote
acceptance-stream: build cli-plugin-install
	./test/acceptance_stream_e2e.sh

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
