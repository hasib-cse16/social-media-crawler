BINARY := socialstats
PKG    := ./cmd/api
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

# Local defaults matching docker-compose.yml. A real DATABASE_URL in the
# environment (or in .env) wins, so these only apply to bare `make` targets.
DATABASE_URL      ?= postgres://socialstats:socialstats@localhost:5432/socialstats?sslmode=disable
TEST_DATABASE_URL ?= postgres://socialstats:socialstats@localhost:5433/socialstats_test?sslmode=disable
export DATABASE_URL

.PHONY: help build run test test-db cover fmt vet lint tidy clean docker docs spec-lint client \
        db-up db-down db-reset db-shell migrate

help:
	@grep -E '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/'

build: ## compile the api binary into ./bin
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) $(PKG)

run: ## run the api locally
	go run $(PKG)

test: ## run all tests with race detection (no database needed)
	go test -race ./...

test-db: ## run integration tests against the disposable test database
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -race -count=1 ./internal/storage/... ./internal/auth/...

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

db-up: ## start postgres (dev on :5432, disposable test db on :5433)
	docker compose up -d postgres postgres-test
	@echo "waiting for postgres..."
	@until docker compose exec -T postgres pg_isready -U socialstats -q; do sleep 0.5; done
	@until docker compose exec -T postgres-test pg_isready -U socialstats -q; do sleep 0.5; done
	@echo "ready:  $(DATABASE_URL)"
	@echo "tests:  $(TEST_DATABASE_URL)"

db-down: ## stop postgres, keeping the dev volume
	docker compose down

db-reset: ## destroy the dev database and rebuild it from migrations
	docker compose down -v
	$(MAKE) db-up
	$(MAKE) migrate

db-shell: ## open psql against the dev database
	docker compose exec postgres psql -U socialstats -d socialstats

migrate: ## apply pending migrations to $$DATABASE_URL
	go run $(PKG) -migrate-only

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
