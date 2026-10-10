.PHONY: build test lint golden e2e

build:
	go build -o kube-rightsize-diff ./cmd/kube-rightsize-diff

test:
	go vet ./...
	go test -race ./...

lint:
	golangci-lint run ./...

# Rewrite report golden files; review the diff before committing.
golden:
	go test ./internal/report -update

# Needs a kind cluster in the current kubectl context.
e2e: build
	KRD_BIN=$(CURDIR)/kube-rightsize-diff ./e2e/run.sh
