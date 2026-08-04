BIN := bin/mock-services

.PHONY: build test test-unit test-e2e vet lint clean run

build:
	go build -o $(BIN) ./cmd/mock-services/

test: test-unit test-e2e

test-unit:
	MOCK_SERVICES_SKIP_E2E=1 go test ./...

test-e2e:
	go test -run TestE2E -v ./internal/aws/

vet:
	go vet ./...

# Requires golangci-lint installed; skipped in CI without it.
lint:
	golangci-lint run ./... || true

run: build
	$(BIN)

clean:
	rm -rf $(BIN)
