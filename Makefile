.PHONY: wasm build-wasm lint test

# Commit (or ref) of github.com/mgilbir/tectonic@wasm to build the WASM module
# from. Pinned to a specific SHA for reproducible artifacts; changing it also
# busts the Docker git-clone layer cache (no --no-cache needed). The current pin
# is the tectonic@wasm merge that made the engine end aborts with a typed
# proc_exit (see errors.go texAbortExitCode).
TECTONIC_REF ?= 488501cb8e4e6c642a1e719a73c4569f6571462d

wasm build-wasm:
	DOCKER_BUILDKIT=1 docker build \
		--build-arg TECTONIC_REF=$(TECTONIC_REF) \
		--target artifact --output type=local,dest=wasm .

lint:
	golangci-lint run ./...

test:
	go test ./...
