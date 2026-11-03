# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.26.10 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X github.com/Arvo-AI/lastwords/internal/agent.Version=${VERSION}" -o /out/lastwords ./cmd/lastwords

# Static binary: it is also copied onto the node and run by the kernel as the
# core dump helper, so it must not depend on anything in this image.
FROM gcr.io/distroless/static-debian12:nonroot
# Lets the MCP registry verify that this image belongs to the listed server.
LABEL io.modelcontextprotocol.server.name="io.github.Arvo-AI/lastwords"
LABEL org.opencontainers.image.source="https://github.com/Arvo-AI/lastwords"
COPY --from=build /out/lastwords /usr/local/bin/lastwords
USER 0:0
ENTRYPOINT ["/usr/local/bin/lastwords"]
CMD ["agent"]
