# syntax=docker/dockerfile:1
#
# docker build --target connector -t edge-connector .
# docker build --target hub -t edge-hub .
#
# Both images are a static binary on distroless, running as a non-root user,
# so customers' image scanners have next to nothing to report.

FROM golang:1.26.10 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot AS hub
COPY --from=build /out/hub /usr/local/bin/edge-hub
COPY --from=build /out/edgectl /usr/local/bin/edgectl
ENTRYPOINT ["/usr/local/bin/edge-hub"]

FROM gcr.io/distroless/static-debian12:nonroot AS connector
COPY --from=build /out/connector /usr/local/bin/edge-connector
ENTRYPOINT ["/usr/local/bin/edge-connector"]
