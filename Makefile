.PHONY: build fmt generate lint test vet verify

build:
	go build ./cmd/...

fmt:
	gofmt -w $$(find cmd internal proto -name '*.go')

generate:
	go generate ./proto
	pwsh -NoProfile -File scripts/proto-lock.ps1

lint:
	golangci-lint run

test:
	go test -race ./...

vet:
	go vet ./...

verify: generate vet lint test build
