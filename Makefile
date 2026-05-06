BIN      := ./dist/bigband
INSTALL  := $(HOME)/bin/bigband
CMD      := ./cmd/bigband
LAUNCHD  := io.bigband.daemon

# Resolve version from git: tag if HEAD is tagged, else short SHA + "-dirty" if working tree is dirty.
VERSION  ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)

.PHONY: build install restart clean test vet

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(CMD)

install: build
	install -m 755 $(BIN) $(INSTALL)
	@echo "Installed $(INSTALL) ($(VERSION))"

restart: install
	$(INSTALL) install

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

clean:
	rm -f $(BIN)
