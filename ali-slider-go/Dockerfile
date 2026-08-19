# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/ali-slider-go ./cmd/server \
    && mkdir -p /runtime/var/artifacts \
    && chmod 0700 /runtime/var/artifacts

FROM --platform=$TARGETPLATFORM rust:1.88.0-bookworm AS v8-build

WORKDIR /src/native/v8runtime
COPY native/v8runtime/Cargo.toml native/v8runtime/Cargo.lock ./
COPY native/v8runtime/.cargo ./.cargo
COPY native/v8runtime/src ./src

RUN cargo build --release --locked

FROM v8-build AS native-test

RUN cargo test --release --locked

FROM --platform=$TARGETPLATFORM golang:1.26.6-bookworm AS v8-test

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
COPY --from=v8-build /src/native/v8runtime/target/release/libali_slider_v8_runtime.so /runtime/libali_slider_v8_runtime.so

RUN CGO_ENABLED=0 \
    ALI_SLIDER_V8_TEST_LIBRARY=/runtime/libali_slider_v8_runtime.so \
    go test -count=1 ./internal/v8runtime ./internal/pe

FROM scratch AS package

COPY --from=build /out/ali-slider-go /ali-slider-go
COPY --from=v8-build /src/native/v8runtime/target/release/libali_slider_v8_runtime.so /libali_slider_v8_runtime.so
COPY native/v8runtime/THIRD-PARTY-NOTICES.txt /THIRD-PARTY-NOTICES.txt

FROM debian:bookworm-slim

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ali-slider-go /app/ali-slider-go
COPY --from=v8-build /src/native/v8runtime/target/release/libali_slider_v8_runtime.so /app/libali_slider_v8_runtime.so
COPY native/v8runtime/THIRD-PARTY-NOTICES.txt /app/THIRD-PARTY-NOTICES.txt
COPY --chown=65532:65532 --from=build /runtime/var /app/var

WORKDIR /app
ENV ALI_SLIDER_V8_LIBRARY=/app/libali_slider_v8_runtime.so
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["/app/ali-slider-go"]
CMD []
