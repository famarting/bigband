BIN              := ./dist/bigband
SLACK_BIN        := ./dist/bigband-slack
WAKE_BIN         := ./dist/bigband-wake

# Install into the same directory `go install` writes to, so a `make install`
# build and a `go install github.com/famarting/bigband/cmd/...@<sha>` cannot
# leave two different bigbands on one machine. That drift is not theoretical:
# it produced a ~/bin copy and a ~/go/bin copy three months and a task->job
# rename apart, with the LaunchAgent pinned to whichever one ran `bigband
# install`. Override with `make INSTALL_DIR=/somewhere/else install`.
INSTALL_DIR      ?= $(shell go env GOBIN)
ifeq ($(strip $(INSTALL_DIR)),)
INSTALL_DIR      := $(shell go env GOPATH)/bin
endif
INSTALL          := $(INSTALL_DIR)/bigband
SLACK_INSTALL    := $(INSTALL_DIR)/bigband-slack
WAKE_INSTALL     := $(INSTALL_DIR)/bigband-wake
CMD              := ./cmd/bigband
SLACK_CMD        := ./cmd/bigband-slack
WAKE_CMD         := ./cmd/bigband-wake
LAUNCHD          := io.bigband.daemon

# Resolve version from git: tag if HEAD is tagged, else short SHA + "-dirty" if working tree is dirty.
VERSION  ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)

.PHONY: build build-slack build-wake build-all install install-slack install-wake install-all restart restart-slack restart-wake clean test vet

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(CMD)

build-slack:
	go build -ldflags "$(LDFLAGS)" -o $(SLACK_BIN) $(SLACK_CMD)

build-wake:
	go build -ldflags "$(LDFLAGS)" -o $(WAKE_BIN) $(WAKE_CMD)

build-all: build build-slack build-wake

install: build
	@mkdir -p $(INSTALL_DIR)
	install -m 755 $(BIN) $(INSTALL)
	@echo "Installed $(INSTALL) ($(VERSION))"

install-slack: build-slack
	@mkdir -p $(INSTALL_DIR)
	install -m 755 $(SLACK_BIN) $(SLACK_INSTALL)
	@echo "Installed $(SLACK_INSTALL) ($(VERSION))"

install-wake: build-wake
	@mkdir -p $(INSTALL_DIR)
	install -m 755 $(WAKE_BIN) $(WAKE_INSTALL)
	@echo "Installed $(WAKE_INSTALL) ($(VERSION))"

install-all: install install-slack install-wake

# Restart the bigband daemon (re-runs `bigband install` which restarts launchd).
restart: install
	$(INSTALL) install

# Restart bigband-slack: replaces the binary, then asks the supervisor to
# bounce the child so the new binary takes effect. The bigband daemon owns
# the process lifecycle; there's no per-extension LaunchAgent any more.
restart-slack: install-slack
	$(INSTALL) ext restart bigband-slack

# Restart bigband-wake: same pattern as restart-slack.
restart-wake: install-wake
	$(INSTALL) ext restart bigband-wake

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

clean:
	rm -f $(BIN) $(SLACK_BIN) $(WAKE_BIN)
