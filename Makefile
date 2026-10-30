.PHONY: check build test lint eval

check: build test lint

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run

eval:
	go run ./cmd/hazard eval
