.PHONY: build-wasm check-wasm-update lint test

TECTONIC_REPO := https://github.com/mgilbir/tectonic.git
TECTONIC_BRANCH := wasm
# Single source of truth for the pinned commit is the Dockerfile's ARG.
TECTONIC_PIN := $(shell sed -n 's/^ARG TECTONIC_COMMIT=//p' Dockerfile)

build-wasm:
	DOCKER_BUILDKIT=1 docker build --target artifact --output type=local,dest=wasm .

# Report whether the fork's wasm branch has advanced past the commit pinned in
# the Dockerfile (ARG TECTONIC_COMMIT).
check-wasm-update:
	@pinned="$(TECTONIC_PIN)"; \
	remote=$$(git ls-remote $(TECTONIC_REPO) refs/heads/$(TECTONIC_BRANCH) | cut -f1); \
	echo "pinned (Dockerfile): $$pinned"; \
	echo "wasm branch HEAD:    $$remote"; \
	if [ -z "$$remote" ]; then \
		echo "error: could not read $(TECTONIC_BRANCH) on $(TECTONIC_REPO)" >&2; exit 1; \
	elif [ "$$pinned" = "$$remote" ]; then \
		echo "Up to date — pinned commit is the wasm branch tip."; \
	else \
		echo "A newer commit exists on $(TECTONIC_BRANCH)."; \
		echo "Bump ARG TECTONIC_COMMIT to $$remote in the Dockerfile, then run 'make build-wasm'."; \
		exit 1; \
	fi

lint:
	golangci-lint run ./...

test:
	go test ./...
