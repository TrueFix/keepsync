VERSION ?= $(shell tr -d '\n' < VERSION)
GO ?= go

.PHONY: build test check fmt image image-rootless image-test clean
build:
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=$(VERSION)" -o bin/keepsync ./cmd/keepsync
test:
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
check:
	test -z "$$($(GO) fmt ./...)"
	$(GO) vet ./...
	$(MAKE) test
fmt:
	$(GO) fmt ./...
image:
	docker build --target runtime --build-arg VERSION=$(VERSION) -t keepsync:test .
image-rootless:
	docker build --target rootless --build-arg VERSION=$(VERSION) -t keepsync:test-rootless .
image-test: image image-rootless
	bash scripts/docker-smoke.sh keepsync:test
	bash scripts/docker-smoke.sh keepsync:test-rootless
clean:
	rm -rf bin dist coverage.out
