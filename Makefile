.PHONY: wasm build-wasm lint test

# Commit of github.com/mgilbir/tectonic@wasm to build the WASM module from.
# Pinned to a specific SHA for reproducible artifacts. Pin a SHA, not a branch
# name: a new SHA busts the Docker git-clone layer cache (no --no-cache needed),
# whereas a branch force-pushed to new content keeps the same ARG value and would
# silently reuse the stale cached clone. The current pin (the wasm branch tip)
# honors SOURCE_DATE_EPOCH for the document date (WithBuildDate) and reports ABI 2
# (see expectedABIVersion).
TECTONIC_REF ?= d1a46a5d127ea40c6a1afcb45e91bb3d7c1f5e47

wasm build-wasm:
	DOCKER_BUILDKIT=1 docker build \
		--build-arg TECTONIC_REF=$(TECTONIC_REF) \
		--target artifact --output type=local,dest=wasm .

lint:
	golangci-lint run ./...

test:
	go test ./...
