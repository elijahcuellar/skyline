# Lint shell scripts in the scripts directory
sc:
    shellcheck ./scripts/*

fmt:
    go fmt ./...
    go mod tidy

# static analysis
vet:
    go vet ./...

# golangci-lint
lint:
    golangci-lint run

# build production binary
build:
    CGO_ENABLED=0 go build -o bin ./...

modernize:
    go run golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize@latest -fix ./...
