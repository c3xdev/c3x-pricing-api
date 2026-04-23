BINARY := c3x-pricing-api
PKG := github.com/c3xdev/c3x-pricing-api/cmd/server

.PHONY: build test test-integration lint vet fmt tidy run scrape-aws seed docker-up docker-down

build:
	CGO_ENABLED=0 go build -o build/$(BINARY) $(PKG)

test:
	go test ./... -race -count=1

# test-integration spins up ephemeral Postgres containers via testcontainers.
# Requires a working Docker daemon.
test-integration:
	go test -tags=integration -count=1 -timeout 5m ./internal/db/...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

lint:
	golangci-lint run

run: build
	./build/$(BINARY) serve

scrape-aws: build
	./build/$(BINARY) scrape --vendor aws

scrape-all: build
	./build/$(BINARY) scrape --vendor all

seed: build
	./build/$(BINARY) seed --file e2e/seed.json

docker-up:
	docker compose up -d

docker-down:
	docker compose down
