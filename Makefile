GO     = /usr/local/go/bin/go
BINARY = samsung-scan
MODULE = ./cmd/samsung-scan
LDFLAGS = -ldflags="-s -w"

.PHONY: build-mac build-linux test test-race lint docker clean

build-mac:
	mkdir -p dist
	GOOS=darwin GOARCH=arm64 $(GO) build $(LDFLAGS) -o dist/$(BINARY)-macos $(MODULE)

build-linux:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(LDFLAGS) -o dist/$(BINARY)-linux $(MODULE)

test:
	$(GO) test ./... -v

test-race:
	$(GO) test -race ./...

lint:
	$(GO) vet ./...

docker:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(BINARY):latest .

clean:
	rm -rf dist
