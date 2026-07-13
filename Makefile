.PHONY: test test-integration vet build compose-config

test:
	go test ./...

test-integration:
	docker-compose -f compose.yaml -f compose.integration.yaml run --rm integration-tests

vet:
	go vet ./...

build:
	go build ./cmd/fluxmaker ./cmd/watchdog ./cmd/admin-api

compose-config:
	docker-compose config -q
