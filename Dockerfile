# syntax=docker/dockerfile:1
#
# docker build --target connector -t edge-connector .
# docker build --target hub -t edge-hub .
# docker buildx build --platform linux/amd64,linux/arm64 --target connector .
#
# Both images are a static binary on distroless, running as a non-root user,
# so customers' image scanners have next to nothing to report. The build
# stage runs on the builder's own platform and cross-compiles, so multi-arch
# images need no emulation.

FROM --platform=$BUILDPLATFORM golang:1.26.10 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ ./cmd/... \
 && mkdir /out/state

FROM gcr.io/distroless/static-debian12:nonroot AS hub
COPY --from=build /out/hub /usr/local/bin/edge-hub
COPY --from=build /out/edgectl /usr/local/bin/edgectl
ENTRYPOINT ["/usr/local/bin/edge-hub"]

FROM gcr.io/distroless/static-debian12:nonroot AS connector
COPY --from=build /out/connector /usr/local/bin/edge-connector
# An enrolled connector's state_dir. A new volume mounted here starts out
# owned by the non-root user, as Docker copies the directory's ownership.
COPY --from=build --chown=65532:65532 /out/state /var/lib/edge-connector
ENTRYPOINT ["/usr/local/bin/edge-connector"]
