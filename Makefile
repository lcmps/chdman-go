.PHONY: test release-check

test:
	go test ./...

# release-check is the release gate. Compatibility tests fail instead of
# skipping when chdman 0.289 is missing, and any skipped test fails the check.
# The race detector needs a C toolchain as a test-time instrument only, which
# is why it is a separate pass and the build itself stays CGO_ENABLED=0.
release-check:
	go vet ./...
	@log="$$(mktemp)"; \
	if ! CGO_ENABLED=0 CHDMAN_GO_RELEASE=1 go test -v ./... >"$$log" 2>&1; then \
		grep -E -- '^(\s*--- FAIL|FAIL|panic)' "$$log"; echo "full log: $$log"; exit 1; \
	fi; \
	if grep -E -- '^\s*--- SKIP' "$$log"; then echo "release-check: tests were skipped, log: $$log"; exit 1; fi; \
	echo "tests passed with no skips"
	CGO_ENABLED=0 go build ./...
	@cgo="$$(CGO_ENABLED=0 go list -f '{{.CgoFiles}}' -deps ./... | sort -u)"; \
	if [ "$$cgo" != "[]" ]; then echo "cgo files in the build: $$cgo"; exit 1; fi; \
	echo "no cgo files in the build"
	go test -race ./...
