.PHONY: check fmt fmt-check vet test

check: fmt-check vet test

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

test:
	go test -race ./...
