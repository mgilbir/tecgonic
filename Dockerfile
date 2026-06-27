# Stage 1: Rust toolchain with wasi-sdk
FROM rust:1.82-bookworm AS toolchain

# Provided automatically by BuildKit (amd64 / arm64).
ARG TARGETARCH

# Install build dependencies
RUN apt-get update && apt-get install -y \
    cmake \
    build-essential \
    autotools-dev \
    python3 \
    wget \
    && rm -rf /var/lib/apt/lists/*

# Install wasi-sdk 25, arch-matched to the build platform so its clang runs
# natively rather than under emulation (the wasm32 output is identical either
# way). wasi-sdk labels releases x86_64/arm64; Docker uses amd64/arm64.
RUN case "${TARGETARCH}" in \
      amd64) WASI_ARCH=x86_64 ;; \
      arm64) WASI_ARCH=arm64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && wget -q "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-25/wasi-sdk-25.0-${WASI_ARCH}-linux.tar.gz" \
    && tar xzf "wasi-sdk-25.0-${WASI_ARCH}-linux.tar.gz" \
    && mv "wasi-sdk-25.0-${WASI_ARCH}-linux" /opt/wasi-sdk \
    && rm "wasi-sdk-25.0-${WASI_ARCH}-linux.tar.gz"

# Add wasm32-wasip1 target
RUN rustup target add wasm32-wasip1

# Stage 2: Clone source
FROM toolchain AS source

RUN git clone --branch wasm --recursive https://github.com/mgilbir/tectonic.git /src/tectonic

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
