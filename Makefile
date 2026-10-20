VERSION ?= $(shell tr -d '\n' < VERSION)
GO ?= go

.PHONY: build test check fmt release clean
build:
	$(GO) build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/keepsync ./cmd/keepsync
test:
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
check:
	test -z "$$($(GO) fmt ./...)"
	$(GO) vet ./...
	$(MAKE) test
fmt:
	$(GO) fmt ./...
release:
	bash scripts/build-release.sh "$(VERSION)"
clean:
	rm -rf bin dist coverage.out
