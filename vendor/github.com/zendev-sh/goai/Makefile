.PHONY: build test lint cover bench docs docs-dev docs-preview clean setup

# --- Go SDK ---

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

bench:
	cd bench && make all

# --- Docs site (VitePress, source in docs/) ---

docs:
	npm install && npx vitepress build docs

docs-dev:
	npm install && npx vitepress dev docs

docs-preview:
	npx vitepress preview docs

# --- Setup ---

setup:
	cp hooks/pre-commit .git/hooks/pre-commit
	cp hooks/commit-msg .git/hooks/commit-msg
	chmod +x .git/hooks/pre-commit .git/hooks/commit-msg
	@echo "Git hooks installed."

# --- Cleanup ---

clean:
	rm -f coverage.out
	rm -rf docs/.vitepress/dist docs/.vitepress/cache node_modules
