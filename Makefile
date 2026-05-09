BIN          := ./dist/bigband
SLACK_BIN    := ./dist/bigband-slack
INSTALL      := $(HOME)/bin/bigband
SLACK_INSTALL:= $(HOME)/bin/bigband-slack
CMD          := ./cmd/bigband
SLACK_CMD    := ./cmd/bigband-slack
LAUNCHD      := io.bigband.daemon

# Resolve version from git: tag if HEAD is tagged, else short SHA + "-dirty" if working tree is dirty.
VERSION  ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)

.PHONY: build build-slack build-all install install-slack install-all restart restart-slack clean test vet

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(CMD)

build-slack:
	go build -ldflags "$(LDFLAGS)" -o $(SLACK_BIN) $(SLACK_CMD)

build-all: build build-slack

install: build
	install -m 755 $(BIN) $(INSTALL)
	@echo "Installed $(INSTALL) ($(VERSION))"

install-slack: build-slack
	install -m 755 $(SLACK_BIN) $(SLACK_INSTALL)
	@echo "Installed $(SLACK_INSTALL) ($(VERSION))"

install-all: install install-slack

# Restart the bigband daemon (re-runs `bigband install` which restarts launchd).
restart: install
	$(INSTALL) install

# Restart bigband-slack: replaces the binary, then asks the supervisor to
# bounce the child so the new binary takes effect. The bigband daemon owns
# the process lifecycle; there's no per-extension LaunchAgent any more.
restart-slack: install-slack
	$(INSTALL) ext restart bigband-slack

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

clean:
	rm -f $(BIN) $(SLACK_BIN)
