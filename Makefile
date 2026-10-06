.PHONY: check fmt fmt-check vet lint test

GOLANGCI_LINT_VERSION := v2.14.0

check: fmt-check vet lint test

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

test:
	go test -race ./...
