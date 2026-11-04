.PHONY: build setup test test-unit test-contract test-monitoring verify bicep

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
	go test -race -tags=integration ./internal/installer -run '^TestPinned(Upstream|UpstreamMonitoring|ImagePublisher|ImageCheck|PodmanPublisher)Contract$$' -count=1

test-monitoring:
	go test -race -tags=integration ./deploy -run '^TestMonitoringCollectorPipeline$$' -count=1

verify:
	test -z "$$(gofmt -l cmd internal infra deploy)"
	test -z "$$(gofmt -l internal/installer/imagecheck/*.go.txt internal/installer/publisher/*.go.txt)"
	go vet ./...
	go mod tidy -diff

bicep:
	az bicep build --file infra/main.bicep --stdout > /dev/null
