.PHONY: test release-check

test:
	go test ./...

# release-check is the release gate. Compatibility tests fail instead of
# skipping when chdman 0.289 is missing, and any skipped test fails the check.
# The race detector needs a C toolchain as a test-time instrument only, which
# is why it is a separate pass and the build itself stays CGO_ENABLED=0. The
# race pass is slow (root package about 10 minutes), so it gets an explicit
# timeout instead of the 10 minute default.
release-check:
	go vet ./...
	@log="$$(mktemp)"; \
	if ! CGO_ENABLED=0 CHDMAN_GO_RELEASE=1 go test -count=1 -timeout 30m -v ./... >"$$log" 2>&1; then \
		grep -E -- '^([[:space:]]*--- FAIL|FAIL|panic)' "$$log"; echo "full log: $$log"; exit 1; \
	fi; \
	if grep -E -- '^[[:space:]]*--- SKIP' "$$log"; then echo "release-check: tests were skipped, log: $$log"; exit 1; fi; \
	rm -f -- "$$log"; \
	echo "tests passed with no skips"
	CGO_ENABLED=0 go build ./...
	@cgo="$$(CGO_ENABLED=1 go list -deps -f '{{if and (not .Standard) .CgoFiles}}{{.ImportPath}} {{.CgoFiles}}{{end}}' ./...)"; \
	if [ -n "$$cgo" ]; then echo "cgo in a non-standard-library package:"; echo "$$cgo"; exit 1; fi; \
	echo "no cgo outside the standard library"
	go test -count=1 -timeout 30m -race ./...
