.PHONY: build-wasm

build-wasm:
	DOCKER_BUILDKIT=1 docker build --target artifact --output type=local,dest=wasm .
