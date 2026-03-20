BINARY  := kiro-bridge-go
INSTALL := $(HOME)/.local/bin/$(BINARY)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.Version=$(VERSION)"

.PHONY: build install

build:
	go build $(LDFLAGS) -o $(BINARY) .

install: build
	mkdir -p $(HOME)/.local/bin
	cp $(BINARY) $(INSTALL)
