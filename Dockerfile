# DriftGate as a container image: one static binary on a minimal base.
#
#   docker build -t driftgate \
#     --build-arg VERSION=v1.0.2 --build-arg COMMIT=$(git rev-parse HEAD) \
#     --build-arg DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) .
#   docker run --rm -v "$PWD:/work" --network host driftgate run
#
# The image holds only driftgate. Commands that driftgate.yaml runs (seed,
# restore, prepare, discover adapters such as php artisan) must exist in the
# image too; for those, build an image FROM yours and COPY the binary from
# this one (COPY --from=driftgate /usr/local/bin/driftgate /usr/local/bin/).

# Base images are pinned by digest (the tag says which version it is); update
# the tag and the digest together:
#   docker buildx imagetools inspect golang:1.27.4-alpine

FROM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
# -s -w: no symbol table and no DWARF (release build).
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/orangehill/driftgate/internal/cli.Version=${VERSION} \
        -X github.com/orangehill/driftgate/internal/cli.Commit=${COMMIT} \
        -X github.com/orangehill/driftgate/internal/cli.Date=${DATE}" \
      -o /out/driftgate ./cmd/driftgate
# Writable folders for the scratch image: the home for the local store and
# /tmp for the temporary files of run.
RUN mkdir -p /out/home/driftgate /out/tmp && chmod 1777 /out/tmp

FROM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f AS certs
RUN apk add --no-cache ca-certificates

# Runtime: CA certificates, the binary, a non-root user and a writable home
# for the local store (~/.driftgate). No shell: commands from driftgate.yaml
# that need one run in an image built FROM the application's own image.
FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/driftgate /usr/local/bin/driftgate
COPY --from=build /out/tmp /tmp
COPY --chown=65532:65532 --from=build /out/home/driftgate /home/driftgate
ENV HOME=/home/driftgate
USER 65532:65532
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/driftgate"]
CMD ["help"]
