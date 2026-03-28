default: lint test

# Run linter
lint:
    golangci-lint run

# Run tests
test *ARGS="./...":
    go test -count=1 {{ARGS}}

# Run tests with the race detector enabled
test-race *ARGS="./...":
    just test -race {{ARGS}}
