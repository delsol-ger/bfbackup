# Uses a local Go toolchain if present, otherwise Go in Docker (scripts/dgo).
GO ?= $(shell command -v go >/dev/null 2>&1 && echo go || echo scripts/dgo)
# Empty: the version in cmd/bfbackup/main.go. Set it only to override.
VERSION ?=
IMAGE ?= bfbackup:latest

.PHONY: test vet race build docker dev-up dev-down

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Race detector needs cgo (Debian-based Go image).
race:
	CGO_ENABLED=1 GO_IMAGE=golang:1.26 scripts/dgo test -race ./...

build:
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -ldflags "-s -w $(if $(VERSION),-X main.version=$(VERSION))" -o dist/bfbackup ./cmd/bfbackup

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# Local controller + peer for trying things out (see docker-compose.yml).
dev-up: docker
	docker compose up -d

dev-down:
	docker compose down
