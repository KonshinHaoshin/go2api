.PHONY: build run test tidy fmt vet clean

BIN ?= bin/go2api
CONFIG ?= configs/config.yaml

build:
	go build -o $(BIN) ./cmd/server

run:
	go run ./cmd/server -config $(CONFIG)

tidy:
	go mod tidy

fmt:
	go fmt ./...

vet:
	go vet ./...

clean:
	rm -rf bin data/*.db