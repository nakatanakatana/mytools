.PHONY: build test lint clean gen-sqlc check-sqlc generate manifests check-litestream-manifests

BINARY_NAME=mytools
BUILD_DIR=dist

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0

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

generate:
	$(CONTROLLER_GEN) object paths=./api/litestream/...

manifests:
	$(CONTROLLER_GEN) rbac:roleName=litestream-controller crd webhook paths="./api/litestream/...;./internal/litestream/controller/...;./internal/litestream/webhook/..." output:crd:artifacts:config=config/litestream-controller/crd/bases output:rbac:artifacts:config=config/litestream-controller/rbac output:webhook:artifacts:config=config/litestream-controller/webhook

check-litestream-manifests:
	bash scripts/check-litestream-manifests.sh
