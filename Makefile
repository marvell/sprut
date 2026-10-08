.PHONY: check fmt fmt-check vet lint test release-check

# Each tool is pinned in its own module, tools/<tool>/go.mod: its dependencies
# stay out of sprut's build and out of the other tools' (a shared module would
# resolve them together and break a tool). Being in tools/*/go.sum also puts
# them in CI's Go cache key, and go tool caches their binaries.
# Add or bump a tool with `go get -tool <pkg>@<version>` run inside tools/<tool>.
TOOL = go tool -modfile=tools/$(1)/go.mod $(1)

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
	$(call TOOL,golangci-lint) run

# Repeated and shuffled, so a test that depends on timing or order fails here.
test:
	go test -race -count=3 -shuffle=on ./...

# The release job only runs on a v* tag, so check its config and a snapshot
# build on every push: the host's binary must report the ldflags version
# (v...-SNAPSHOT-...), not the ReadBuildInfo fallback.
release-check:
	$(call TOOL,actionlint)
	$(call TOOL,goreleaser) check
	$(call TOOL,goreleaser) build --snapshot --clean
	@out=$$(dist/sprut_$$(go env GOOS)_$$(go env GOARCH)_*/sprut --version) || exit 1; \
	echo "$$out"; \
	case "$$out" in "sprut version v"*-SNAPSHOT-*) ;; \
	*) echo "snapshot build does not report the ldflags version"; exit 1 ;; esac
