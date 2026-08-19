BIN     = wake
PREFIX  ?= $(HOME)/.local
GOFLAGS ?=

.PHONY: all build install clean vet fixture

all: build

build:
	go build $(GOFLAGS) -o $(BIN) .

install:
	go install $(GOFLAGS) .

uninstall:
	rm -f $(PREFIX)/bin/$(BIN)

clean:
	rm -f $(BIN)

vet:
	go vet ./...

fixture:
	./test-fixture
