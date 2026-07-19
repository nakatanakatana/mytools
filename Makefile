.PHONY: build test lint clean gen-sqlc check-sqlc

BINARY_NAME=mytools
BUILD_DIR=dist

build:
	mkdir -p $(BUILD_DIR)
	go build -o ./$(BUILD_DIR)/ ./cmd/...

test:
	go test -v ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BUILD_DIR)

gen-sqlc:
	sqlc generate

check-sqlc: gen-sqlc
	@untracked="$$(git ls-files --others --exclude-standard -- cmd/nostr-bridge/store/sqlc cmd/nostr-relay/store/sqlc)"; \
	if [ -n "$$untracked" ]; then \
		echo "Untracked sqlc generated files:" >&2; \
		printf '%s\n' "$$untracked" >&2; \
		exit 1; \
	fi
	git diff --exit-code -- cmd/nostr-bridge/store/sqlc cmd/nostr-relay/store/sqlc
