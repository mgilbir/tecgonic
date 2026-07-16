.PHONY: wasm build-wasm lint test

# Commit of github.com/mgilbir/tectonic@wasm to build the WASM module from.
# Pinned to a specific SHA for reproducible artifacts. Pin a SHA, not a branch
# name: a new SHA busts the Docker git-clone layer cache (no --no-cache needed),
# whereas a branch force-pushed to new content keeps the same ARG value and would
# silently reuse the stale cached clone. The current pin honors SOURCE_DATE_EPOCH
# for the document date (WithBuildDate) and reports ABI 2 (see expectedABIVersion).
TECTONIC_REF ?= 985168748d499fe87896f83fdb223fd81ed9e047

wasm build-wasm:
	DOCKER_BUILDKIT=1 docker build \
		--build-arg TECTONIC_REF=$(TECTONIC_REF) \
		--target artifact --output type=local,dest=wasm .

lint:
	golangci-lint run ./...

test:
	go test ./...
