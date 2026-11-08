set positional-arguments

default:
    just --list

# Install this clone's git hooks: commit messages must be Conventional Commits.
hooks:
    git config core.hooksPath .githooks
    @echo "Installed .githooks (commit-msg)."

build:
    mkdir -p dist
    go build -trimpath -o dist/cake-monitor ./cmd/cake-monitor

version-check: build
    ./dist/cake-monitor -version | grep -q '^revision '

# Build, install the CLI, and link the four toolbox tools.
install: build
    go install -trimpath ./cmd/cake-monitor
    ./dist/cake-monitor install-tools

# Link the four toolbox names into cake's global toolbox directory.
install-tools: build
    ./dist/cake-monitor install-tools

uninstall-tools: build
    ./dist/cake-monitor uninstall-tools

doctor: build
    ./dist/cake-monitor doctor

test:
    go test ./...

test-race:
    go test -race ./...

fmt:
    gofmt -w .

fmt-check:
    test -z "$(gofmt -l .)"

vet:
    go vet ./...

ci: fmt-check vet test build version-check
