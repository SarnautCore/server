.PHONY: build fmt generate lint test vet verify

build:
	go build ./cmd/...

fmt:
	gofmt -w $$(find cmd internal proto -name '*.go')

generate:
	go generate ./proto

lint:
	golangci-lint run

test:
	go test ./...

vet:
	go vet ./...

verify: generate vet lint test build
