.PHONY: test bench lint fmt fmt-check vet cover clean help

## test: Run all tests with race detector
test:
	go test -race -count=1 ./...

## bench: Run all benchmarks
bench:
	go test -bench=. -benchmem -count=1 ./...

## lint: Run golangci-lint
lint:
	golangci-lint run

## fmt: Format all Go files
fmt:
	gofmt -w .
	goimports -w .

## fmt-check: Check formatting (CI)
fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "Files need formatting:"; gofmt -l .; exit 1)

## vet: Run go vet
vet:
	go vet ./...

## cover: Generate coverage report
cover:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## clean: Remove build artifacts
clean:
	rm -f coverage.out coverage.html
	go clean -testcache

## help: Show this help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | column -t -s ':'
