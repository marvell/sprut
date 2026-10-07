.PHONY: check fmt fmt-check vet lint test release-check

GOLANGCI_LINT_VERSION := v2.14.0
GORELEASER_VERSION := v2.18.2
ACTIONLINT_VERSION := v1.7.12

check: fmt-check vet lint test release-check

fmt:
	gofmt -w .

fmt-check:
	@unformatted=$$(gofmt -l .) || exit 1; \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	go vet ./...

# Built with the local Go toolchain, so it always understands go.mod's Go version.
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

# Repeated and shuffled, so a test that depends on timing or order fails here.
test:
	go test -race -count=3 -shuffle=on ./...

# The release job only runs on a v* tag, so check its config and a snapshot
# build on every push: the host's binary must report the ldflags version
# (v...-SNAPSHOT-...), not the ReadBuildInfo fallback.
release-check:
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) check
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) build --snapshot --clean
	@out=$$(dist/sprut_$$(go env GOOS)_$$(go env GOARCH)_*/sprut --version) || exit 1; \
	echo "$$out"; \
	case "$$out" in "sprut version v"*-SNAPSHOT-*) ;; \
	*) echo "snapshot build does not report the ldflags version"; exit 1 ;; esac
