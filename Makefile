GO ?= go

.PHONY: build test run

build:
	$(GO) build -o bin/volc-stt-proxy .

test:
	$(GO) test ./...

run: build
	./bin/volc-stt-proxy
