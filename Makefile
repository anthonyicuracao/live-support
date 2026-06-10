VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := dist/live-support
PKG     := live-support-$(VERSION)-linux-amd64

.PHONY: build build-linux package run test vet fmt tidy clean

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) .

# Single static Linux/amd64 binary (pure-Go sqlite => no cgo needed).
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)-linux-amd64 .

# Release bundle: the single static binary + env template + systemd unit +
# installer + updater + proxy examples + docs, as one tarball for delivery.
package: build-linux
	rm -rf dist/$(PKG)
	mkdir -p dist/$(PKG)
	cp dist/live-support-linux-amd64 dist/$(PKG)/live-support
	cp .env.example README.md dist/$(PKG)/
	cp deploy/live-support.service deploy/install.sh deploy/update.sh dist/$(PKG)/
	cp deploy/live-support-update.service deploy/live-support-update.timer dist/$(PKG)/
	cp deploy/Caddyfile.example deploy/nginx-live-support.conf.example dist/$(PKG)/
	chmod +x dist/$(PKG)/live-support dist/$(PKG)/install.sh dist/$(PKG)/update.sh
	tar -C dist -czf dist/$(PKG).tar.gz $(PKG)
	@echo "packaged: dist/$(PKG).tar.gz"
	@ls -lh dist/$(PKG).tar.gz

run:
	go run .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -rf dist
