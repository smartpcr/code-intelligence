.PHONY: build clean test-e2e

# build compiles the cleanc CLI binary into bin/cleanc.
# Acceptance scenario: `make -C services/clean-code build` produces
# services/clean-code/bin/cleanc.
build:
	@mkdir -p bin
	go build -o bin/cleanc ./cmd/cleanc

# test-e2e runs the E2E acceptance tests (behind the `e2e` build tag).
# Pre-requisite: `make build` (wired as a dependency).
test-e2e: build
	go test -tags e2e ./test/e2e/... -count=1

clean:
	rm -rf bin/