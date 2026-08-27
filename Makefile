BINARY := socialstats
PKG    := ./cmd/api
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: help build run test cover fmt vet lint tidy clean docker docs spec-lint client

help:
	@grep -E '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/'

build: ## compile the api binary into ./bin
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) $(PKG)

run: ## run the api locally
	go run $(PKG)

test: ## run all tests with race detection
	go test -race ./...

cover: ## run tests and open a coverage summary
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

fmt: ## format all code
	gofmt -w .

vet: ## run go vet
	go vet ./...

lint: fmt vet test ## format, vet and test

tidy: ## tidy modules
	go mod tidy

clean:
	rm -rf bin coverage.out

docs: ## print where the api reference is served
	@echo "Swagger UI:   http://localhost:8080/docs"
	@echo "OpenAPI spec: http://localhost:8080/openapi.yaml"
	@echo "Spec source:  internal/docs/openapi.yaml"

spec-lint: ## validate the openapi spec (requires npx)
	npx --yes @redocly/cli@latest lint internal/docs/openapi.yaml

client: ## generate a typescript client from the spec (requires npx)
	npx --yes @openapitools/openapi-generator-cli generate \
	  -i internal/docs/openapi.yaml -g typescript-fetch -o clients/typescript

docker: ## build the container image
	docker build -t $(BINARY):$(VERSION) .
