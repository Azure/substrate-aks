.PHONY: build setup test test-unit test-contract verify bicep

build:
	go build -o bin/substrate-aks ./cmd/substrate-aks

# Build, then run guided setup; pass flags with ARGS, for example
# make setup ARGS="substrate-dev --accessible".
setup: build
	bin/substrate-aks setup $(ARGS)

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
