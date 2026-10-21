VERSION ?= $(shell tr -d '\n' < VERSION)
GO ?= go

.PHONY: build test check fmt image image-test clean
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
	docker build --build-arg VERSION=$(VERSION) -t keepsync:test .
image-test: image
	bash scripts/docker-smoke.sh keepsync:test
clean:
	rm -rf bin dist coverage.out
