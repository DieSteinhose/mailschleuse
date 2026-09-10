# syntax=docker/dockerfile:1

# --- build ------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# The module has no external dependencies, so this layer stays cached unless
# go.mod itself changes.
COPY go.mod ./
RUN go mod download

COPY . .

# CGO is off and timezone data is embedded so the result runs on scratch.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags timetzdata \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/mailschleuse ./cmd/mailschleuse

# Pre-create the data directory with the runtime UID: a named volume inherits
# ownership from the image, which keeps persistence working for a non-root user.
RUN mkdir -p /data && chown 65532:65532 /data

# --- runtime ----------------------------------------------------------------
FROM scratch

LABEL org.opencontainers.image.title="Mailschleuse" \
      org.opencontainers.image.description="Self-contained SMTP/POP3 mail sink with a web UI for development and test environments" \
      org.opencontainers.image.source="https://github.com/DieSteinhose/Mailschleuse" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/mailschleuse /mailschleuse
COPY --from=build --chown=65532:65532 /data /data

# HTTP UI/API, SMTP submission, POP3 retrieval.
EXPOSE 8080 1025 1110

USER 65532:65532
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/mailschleuse", "-healthcheck"]

ENTRYPOINT ["/mailschleuse"]
