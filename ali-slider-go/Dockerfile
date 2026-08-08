# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/ali-slider-go ./cmd/server \
    && mkdir -p /runtime/var/artifacts \
    && chmod 0700 /runtime/var/artifacts

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ali-slider-go /app/ali-slider-go
COPY --chown=65532:65532 --from=build /runtime/var /app/var

WORKDIR /app
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["/app/ali-slider-go"]
