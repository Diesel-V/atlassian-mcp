VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: test test-integration lint build image

test:
	go test -race ./...

# Requires a disposable PostgreSQL, e.g.:
#   docker run --rm -d -e POSTGRES_PASSWORD=p -p 55432:5432 postgres:17
test-integration:
	TEST_DATABASE_URL=postgres://postgres:p@localhost:55432/postgres?sslmode=disable go test -race -count=1 ./...

lint:
	gofmt -l . | (! grep .)
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/atlassian-mcp ./cmd/atlassian-mcp

image:
	docker build --build-arg VERSION=$(VERSION) -t atlassian-mcp:$(VERSION) .
