.PHONY: build test test-unit test-contract verify bicep

build:
	go build -o bin/substrate-aks ./cmd/substrate-aks

test: test-unit test-contract

test-unit:
	go test -race ./...

test-contract:
	go test -race -tags=integration ./internal/installer -run '^TestPinned(Upstream|ImagePublisher|ImageCheck|PodmanPublisher)Contract$$' -count=1

verify:
	test -z "$$(gofmt -l cmd internal infra deploy)"
	test -z "$$(gofmt -l internal/installer/imagecheck/*.go.txt internal/installer/publisher/*.go.txt)"
	go vet ./...
	go mod tidy -diff

bicep:
	az bicep build --file infra/main.bicep --stdout > /dev/null
