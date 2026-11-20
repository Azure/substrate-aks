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

CONTRACT_TESTS := TestPinned(Upstream|UpstreamFlags|TransferPlaceholder|ReleaseInventory|MicroVMAssets|ManagedPostgres|ImagePublisher|ImageCheck|PodmanPublisher)Contract
CONTRACT_TESTS := $(CONTRACT_TESTS)|TestPinnedDatabaseBootstrapScript|TestManagedBicep(NetworkGrantScope|VaultContract|DatabaseSKU)|TestInfrastructureDeploymentHistorySelection

test-contract:
	go test -race -tags=integration ./internal/installer -run '^($(CONTRACT_TESTS))$$' -count=1

verify:
	test -z "$$(gofmt -l cmd internal infra deploy)"
	test -z "$$(gofmt -l internal/installer/helper/*.go.txt internal/installer/publisher/*.go.txt)"
	go vet ./...
	go mod tidy -diff

bicep:
	az bicep build --file infra/main.bicep --stdout > /dev/null
