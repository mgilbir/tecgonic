.PHONY: wasm build-wasm lint test

# Commit (or ref) of github.com/mgilbir/tectonic@wasm to build the WASM module
# from. Pin a specific SHA for reproducible artifacts; changing it also busts
# the Docker git-clone layer cache (no --no-cache needed).
TECTONIC_REF ?= wasm

wasm build-wasm:
	DOCKER_BUILDKIT=1 docker build \
		--build-arg TECTONIC_REF=$(TECTONIC_REF) \
		--target artifact --output type=local,dest=wasm .

lint:
	golangci-lint run ./...

test:
	go test ./...
