# syntax=docker/dockerfile:1

# Base images are pinned by digest as well as tag: the tag documents what the
# digest is, and the digest is what makes a rebuild reproducible.
ARG BUILD_IMAGE=golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35

FROM ${BUILD_IMAGE} AS build

WORKDIR /src

# The module manifests are copied first so the download layer is reused while
# only application code changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# VERSION is the commit SHA the image is tagged with, so a running container can
# report the source it was built from via `pacapulse -version`.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

# CGO is disabled to produce a static binary that runs on a distroless base. The
# SQLite driver is a pure-Go translation of SQLite for exactly this reason: the
# database needs no shared library, and the runtime image needs no libc.
# -trimpath removes local filesystem paths and -buildvcs=false keeps the build
# independent of whether the build context carries a .git directory.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -X github.com/mtrenker/pacapulse/internal/version.version=${VERSION}" \
      -o /out/pacapulse \
      ./cmd/pacapulse

FROM ${RUNTIME_IMAGE}

ARG VERSION=dev
LABEL org.opencontainers.image.title="PacaPulse" \
      org.opencontainers.image.description="Lightweight, privacy-first web analytics for multiple sites." \
      org.opencontainers.image.source="https://github.com/mtrenker/pacapulse" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.revision="${VERSION}"

COPY --from=build /out/pacapulse /usr/local/bin/pacapulse

# The distroless nonroot variant runs as uid 65532; stating it here keeps the
# guarantee visible in `docker image inspect` and in review.
USER nonroot:nonroot

ENV PACAPULSE_ADDR=:8080
# The database lives on a mounted volume, not in the container's writable layer:
# the image deliberately does not create /data, so a container started without a
# volume fails loudly instead of writing analytics somewhere that disappears
# with it.
ENV PACAPULSE_DATA_DIR=/data
EXPOSE 8080

# The runtime image has no shell and no curl, so the binary probes itself. This
# is the same code path the local smoke test and Kubernetes probes exercise.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/pacapulse", "-health-check"]

ENTRYPOINT ["/usr/local/bin/pacapulse"]
