# loom-cloud — Makefile
#
# Mirrors the loom repo's Makefile. `make ci` runs the same
# checks the workflow runs (tidy + vet + build + test) so
# contributors catch drift locally before pushing.

.PHONY: tidy vet build test ci pre-commit help

.DEFAULT_GOAL := help

help:
	@echo "loom-cloud Makefile — common targets:"
	@echo ""
	@echo "  make tidy        Run 'go mod tidy' (commit the result)"
	@echo "  make vet         go vet ./..."
	@echo "  make build       go build ./..."
	@echo "  make test        go test -race ./..."
	@echo "  make ci          Full CI parity: tidy + vet + build + test"
	@echo "  make pre-commit  Same as ci, plus checks the tree is clean"
	@echo ""

tidy:
	go mod tidy

vet:
	go vet ./...

build:
	go build ./...

test:
	go test -race ./...

ci: tidy vet build test
	@if ! git diff --exit-code -- go.mod go.sum >/dev/null 2>&1; then \
		echo ""; \
		echo "✗ go.mod / go.sum drifted after tidy. Commit the changes:"; \
		git diff --stat go.mod go.sum; \
		exit 1; \
	fi
	@echo ""
	@echo "✓ make ci: full parity check passed"

pre-commit: ci
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo ""; \
		echo "✗ working tree has uncommitted changes after ci checks:"; \
		git status --short; \
		exit 1; \
	fi
	@echo "✓ make pre-commit: tree is clean"
