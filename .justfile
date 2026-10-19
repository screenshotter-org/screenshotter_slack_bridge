# Run lint, check-format, and test (default target).
default: lint check-format test

# Run all tests.
test:
    go test ./...

# Run go vet and staticcheck.
# GOTOOLCHAIN pins the toolchain staticcheck analyses with: v0.8.1 cannot parse
# the Go 1.27 standard library this module's toolchain directive selects. Drop
# the pin once a staticcheck release supports Go 1.27 (see issue #50).
# The staticcheck version is updated by Renovate, through a custom manager in
# renovate.json; the GOTOOLCHAIN pin above is deliberately left to us.
lint:
    go vet ./...
    GOTOOLCHAIN=go1.26.8 go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

# Format all Go source files in place.
format:
    gofmt -w .

# Check formatting without modifying files; fails if any file is unformatted.
check-format:
    @test -z "$(gofmt -l .)" || \
        (echo "The following files are not formatted (run 'just format' to fix):" && \
         gofmt -l . && exit 1)

# Download Go module dependencies.
deps:
    go mod download

# Scan dependencies for known vulnerabilities (same pinned tool as CI).
audit:
    go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...

# Update dependencies to their latest patch releases, then tidy and verify.
# Review the go.mod/go.sum diff and run the tests before committing.
update:
    go get -u=patch ./...
    go mod tidy
    go mod verify

alias all := build

# Build the bridge binary.
build:
    go build -o screenshotter-slack-bridge ./cmd/screenshotter-slack-bridge

# Install the binary into $GOBIN (or $GOPATH/bin).
install:
    go install ./cmd/screenshotter-slack-bridge

# Remove the installed binary from $GOBIN (or $GOPATH/bin).
uninstall:
    #!/usr/bin/env bash
    set -euo pipefail
    bin=$(go env GOBIN)
    [[ -z "$bin" ]] && bin="$(go env GOPATH)/bin"
    rm -f "$bin/screenshotter-slack-bridge"
    echo "Removed $bin/screenshotter-slack-bridge"

# Run the local binary with config.toml (builds first).
run: build check-config
    ./screenshotter-slack-bridge -config config.toml

# Fail with a clear message if config.toml is missing.
[private]
check-config:
    @test -f config.toml || \
        (echo "error: config.toml not found — copy config.example.toml and edit it" >&2 && exit 1)
