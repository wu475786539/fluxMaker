.PHONY: test test-java test-all test-integration vet build build-java compose-config compose-config-java docker-rebuild

test:
	go test ./...

test-java:
	mvn -f java/pom.xml test

test-all: test test-java

test-integration:
	docker-compose -f compose.yaml -f compose.integration.yaml run --rm integration-tests

vet:
	go vet ./...

build:
	go build ./cmd/fluxmaker ./cmd/watchdog ./cmd/admin-api

build-java:
	mvn -f java/pom.xml package

compose-config:
	docker-compose config -q

compose-config-java:
	BACKEND_IMPL=java docker-compose config -q

docker-rebuild:
	./scripts/rebuild-local.sh
