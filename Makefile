# MatchUp backend. One module, four binaries: api, admin, worker, cli.
#
# The sqlc and goose binaries live in the module cache's bin directory rather
# than on PATH, so every target that needs them names them explicitly. Run
# `make tools` once to install the pinned versions.

GOBIN      := $(shell go env GOPATH)/bin
GOOSE      := $(GOBIN)/goose
SQLC       := $(GOBIN)/sqlc
SWAG       := $(GOBIN)/swag
GOOSE_VER  := v3.26.0
SQLC_VER   := v1.31.1
SWAG_VER   := v1.16.6

# Overridable: `make migrate-up DATABASE_URL=...`. The default points at the
# compose Postgres on its host port, not at whatever .env holds -- make does not
# read .env, so a non-default database has to be passed in or exported.
DATABASE_URL ?= postgres://matchup:localdev@localhost:55432/matchup?sslmode=disable

BINARIES := api admin worker cli

.DEFAULT_GOAL := help
.PHONY: help env tools build $(addprefix run-,$(BINARIES)) generate docs docs-fmt \
        migrate-up migrate-down migrate-redo migrate-status migrate-create \
        test test-race test-integration cover fmt fmt-check vet check tidy \
        docker-build up down logs ps psql clean

help: ## List the available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

env: ## Create .env from .env.example if it does not exist
	@test -f .env && echo ".env already exists, leaving it alone" \
		|| { cp .env.example .env; echo "wrote .env -- replace the placeholder secrets"; }

tools: ## Install the pinned sqlc, goose and swag binaries
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VER)
	go install github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VER)
	go install github.com/swaggo/swag/cmd/swag@$(SWAG_VER)

build: ## Build all four binaries into bin/
	@mkdir -p bin
	$(foreach b,$(BINARIES),go build -trimpath -o bin/$(b) ./cmd/$(b) &&) true

run-api: ## Run the public API from source
	go run ./cmd/api

run-admin: ## Run the admin API from source
	go run ./cmd/admin

run-worker: ## Run the background worker from source
	go run ./cmd/worker

run-cli: ## Run the operator CLI: make run-cli ARGS="promote --email a@b.com"
	go run ./cmd/cli $(ARGS)

generate: ## Regenerate internal/db from migrations and queries
	$(SQLC) generate

docs-fmt: ## Format the swag annotations on the handlers
	$(SWAG) fmt -d ./

docs: ## Regenerate the OpenAPI 3 specs in internal/openapi from handler annotations
	# Staged under .openapi/ (gitignored): swag emits Swagger 2.0, and the tool
	# converts it to the OpenAPI 3 documents that are committed under
	# internal/openapi. The two scans are scoped so neither spec picks up the
	# other binary's operations: the public API has no admin handlers at all
	# (directory exclusion), while the admin scan drops every operation tagged
	# "public", because swag's --exclude has no file granularity. That marker
	# tag is stripped from the public document again on conversion, so it never
	# reaches a reader.
	rm -rf .openapi
	$(SWAG) init -g cmd/api/main.go -o .openapi/api --ot json --parseInternal --quiet \
		--exclude ./internal/admin
	$(SWAG) init -g cmd/admin/main.go -o .openapi/admin --ot json --parseInternal --quiet \
		--tags "!public"
	go run ./tools/openapigen -in .openapi/api/swagger.json -out internal/openapi/api.json -server /v1 -strip-tags public
	go run ./tools/openapigen -in .openapi/admin/swagger.json -out internal/openapi/admin.json -server /admin/v1
	rm -rf .openapi

migrate-up: ## Apply every pending migration
	$(GOOSE) -dir migrations postgres "$(DATABASE_URL)" up

migrate-down: ## Roll back the most recent migration
	$(GOOSE) -dir migrations postgres "$(DATABASE_URL)" down

migrate-redo: ## Roll back and reapply the most recent migration
	$(GOOSE) -dir migrations postgres "$(DATABASE_URL)" redo

migrate-status: ## Show which migrations have been applied
	$(GOOSE) -dir migrations postgres "$(DATABASE_URL)" status

migrate-create: ## Create a migration: make migrate-create name=add_thing
	@test -n "$(name)" || { echo "usage: make migrate-create name=add_thing"; exit 1; }
	$(GOOSE) -dir migrations create $(name) sql

test: ## Run the unit tests
	go test ./...

test-race: ## Run the unit tests under the race detector
	go test -race ./...

test-integration: ## Run the tests that need a database (TEST_DATABASE_URL)
	# -p 1 is required, not a preference: the harness truncates the whole schema
	# for every test, so two package binaries sharing one database would delete
	# each other's rows mid-run.
	TEST_DATABASE_URL="$(DATABASE_URL)" go test -race -count=1 -p 1 -tags=integration ./...

cover: ## Write a coverage profile and print the total
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

fmt: ## Format the source
	gofmt -w cmd internal

fmt-check: ## Fail if anything is unformatted
	@out=$$(gofmt -l cmd internal); test -z "$$out" || { echo "unformatted:"; echo "$$out"; exit 1; }

vet: ## Run go vet
	go vet ./...

check: fmt-check vet test ## Everything CI should run

tidy: ## Prune and record dependencies
	go mod tidy

docker-build: ## Build the container image
	docker build -t matchup:local .

up: ## Start Postgres, run migrations, start all three services
	docker compose up --build

down: ## Stop the stack, keeping the database volume
	docker compose down

logs: ## Follow the service logs
	docker compose logs -f api admin worker

ps: ## Show the stack's status
	docker compose ps

psql: ## Open a psql shell on the compose database
	docker compose exec postgres psql -U matchup -d matchup

clean: ## Remove build and coverage output
	rm -rf bin coverage.out
