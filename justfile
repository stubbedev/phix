# phix dev tasks.

# List all recipes with their descriptions.
default:
    @just --list

# Run every release gate in order: vet, lint, test, build.
check: vet lint test build

# Static analysis of every package with go vet.
vet:
    go vet ./...

# Lint every package; settings live in .golangci.yml.
lint:
    golangci-lint run

# Run the test suite for every package.
test:
    go test ./...

# Compile-check every package; the output is discarded.
build:
    go build -o /dev/null ./...

# Format every Go source in place with gofmt.
fmt:
    gofmt -w .

# Recompute the vendorHash in nix/package.nix from go.mod and go.sum.
vendor-hash:
    nix run github:stubbedev/lys-ci/f5bf1ded8f2fbff93f695f91d9884dfccea0a62e#vendor-hash -- nix/package.nix .#default.goModules
