.PHONY: wasm build-wasm lint test

# Commit of github.com/mgilbir/tectonic@wasm to build the WASM module from.
# Pinned to a specific SHA for reproducible artifacts. Pin a SHA, not a branch
# name: a new SHA busts the Docker git-clone layer cache (no --no-cache needed),
# whereas a branch force-pushed to new content keeps the same ARG value and would
# silently reuse the stale cached clone. The current pin adds the
# tectonic_abi_version export checked by New (see expectedABIVersion).
TECTONIC_REF ?= 2cd6b68b9a95ae88c7d4e414f9ab2c6d41a3c13a

wasm build-wasm:
	DOCKER_BUILDKIT=1 docker build \
		--build-arg TECTONIC_REF=$(TECTONIC_REF) \
		--target artifact --output type=local,dest=wasm .

lint:
	golangci-lint run ./...

test:
	go test ./...
