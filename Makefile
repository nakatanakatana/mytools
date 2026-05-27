.PHONY: build test lint clean

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
