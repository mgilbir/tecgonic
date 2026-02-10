.PHONY: build-wasm lint test

build-wasm:
	DOCKER_BUILDKIT=1 docker build --target artifact --output type=local,dest=wasm .

lint:
	golangci-lint run ./...

test:
	go test ./...
