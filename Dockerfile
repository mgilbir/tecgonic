# Stage 1: Rust toolchain with wasi-sdk
FROM rust:1.82-bookworm AS toolchain

# Install build dependencies
RUN apt-get update && apt-get install -y \
    cmake \
    build-essential \
    autotools-dev \
    python3 \
    wget \
    && rm -rf /var/lib/apt/lists/*

# Install wasi-sdk 25
RUN wget -q https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-25/wasi-sdk-25.0-x86_64-linux.tar.gz \
    && tar xzf wasi-sdk-25.0-x86_64-linux.tar.gz \
    && mv wasi-sdk-25.0-x86_64-linux /opt/wasi-sdk \
    && rm wasi-sdk-25.0-x86_64-linux.tar.gz

# Add wasm32-wasip1 target
RUN rustup target add wasm32-wasip1

# Stage 2: Clone source
FROM toolchain AS source

# TECTONIC_REF pins the commit/branch built. Passing a specific SHA makes the
# artifact reproducible and busts this layer's cache when it changes, so a
# rebuild after an upstream push no longer silently reuses a stale clone.
ARG TECTONIC_REF=wasm
RUN git clone --recursive https://github.com/mgilbir/tectonic.git /src/tectonic \
    && git -C /src/tectonic checkout "${TECTONIC_REF}" \
    && git -C /src/tectonic submodule update --init --recursive

WORKDIR /src/tectonic

# Stage 3: Build WASI dependencies (zlib, libpng, FreeType2, Graphite2, ICU)
FROM source AS deps

WORKDIR /src/tectonic
RUN cd wasi-deps && bash build-wasi-deps.sh

# Stage 4: Build tectonic WASM module
FROM deps AS build

WORKDIR /src/tectonic
RUN bash build-wasi.sh

# Stage 5: Extract just the WASM artifact
FROM scratch AS artifact

COPY --from=build /src/tectonic/target/wasm32-wasip1/release/tectonic_wasi.wasm /tectonic_wasi.wasm
