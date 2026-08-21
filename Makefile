.PHONY: build fmt generate lint test vet verify

build:
	go build ./cmd/...

fmt:
	gofmt -w $$(find cmd internal proto -name '*.go')

generate:
	go generate ./proto
	pwsh -NoProfile -File scripts/proto-lock.ps1

lint:
	go tool -modfile=golangci-lint.mod github.com/golangci/golangci-lint/v2/cmd/golangci-lint run

test:
	go test -race ./...

vet:
	go vet ./...

verify: generate vet lint test build
