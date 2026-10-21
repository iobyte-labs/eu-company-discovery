# A static binary in a distroless image.
#
# CGO is off and the SQLite driver is pure Go (modernc.org/sqlite), so the result has no
# libc dependency at all. That is what makes the runtime stage this small and the cold start
# this fast — which is the whole reason the engine is Go rather than Python: on free CI
# minutes, startup time and resident memory are the bill.

FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

ARG VERSION=0.1.0-dev
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w -X github.com/iobyte-labs/eu-company-discovery.Version=${VERSION}" \
      -o /out/eucd ./cmd/eucd

# Prove the binary runs before shipping it: a broken build caught here beats one caught in
# a workflow.
RUN /out/eucd version

# ---------------------------------------------------------------------------

# nonroot carries CA certificates (needed for HTTPS) and /etc/passwd, and nothing else.
# No shell, no package manager, nothing to pivot to.
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="eu-company-discovery" \
      org.opencontainers.image.description="Company discovery through industry association directories" \
      org.opencontainers.image.source="https://github.com/iobyte-labs/eu-company-discovery" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/eucd /usr/local/bin/eucd
# Example configuration. Mount your own over /config in real use.
COPY --from=build /src/config /config

# /data is where the database belongs; mount a volume there. The image itself is read-only
# in practice, and the process runs unprivileged.
WORKDIR /data
USER nonroot:nonroot

ENV EUCD_CONFIG_DIR=/config \
    EUCD_DB=/data/eucd.db

ENTRYPOINT ["/usr/local/bin/eucd"]
CMD ["--help"]
