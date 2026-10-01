# Control plane image.
#
# Two stages. The final image contains the binary, CA certificates and nothing
# else -- no shell, no package manager, no Go toolchain. A compromised process
# therefore has no interpreter to pivot with, and there is no `apt` with which
# to fetch one.
#
# Build from services/control-api:
#   docker build -t vantage/control-api .

# ---------------------------------------------------------------- build stage
FROM golang:1.27.0-alpine3.23 AS build

# git is needed for module resolution of any VCS-hosted dependency.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Dependencies first, so a source-only change does not re-download modules.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# Version metadata is injected rather than hardcoded, so /version reports the
# build that is actually running.
ARG VERSION=0.1.0
ARG COMMIT=unknown

# CGO off for a static binary; trimpath strips local filesystem paths out of
# the binary, which would otherwise leak the build machine's layout.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X github.com/vantage/control-api/internal/app.Version=${VERSION} -X github.com/vantage/control-api/internal/app.Commit=${COMMIT}" \
    -o /out/vantage-control-api ./cmd/control-api

# Fail the build if the binary was somehow produced with live execution
# enabled. The constant is compile-time false; this asserts it survived.
RUN /out/vantage-control-api --help 2>&1 | grep -qi "paper" || \
    (echo "REFUSING TO BUILD: the binary does not identify itself as paper-only" && exit 1)

# ---------------------------------------------------------------- final stage
FROM scratch

# Certificates for outbound TLS (the research service, and any future
# provider). The IANA time zone database is not copied here: it is embedded in
# the binary via `time/tzdata`, so the market clock cannot fail on a missing
# /usr/share/zoneinfo in a scratch image.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=build /out/vantage-control-api /vantage-control-api

# A fixed non-root uid. There is no /etc/passwd in a scratch image, so the
# numeric form is the only correct one.
USER 65532:65532

EXPOSE 8080
ENV TZ=UTC

# The healthcheck is the binary probing itself, because there is no curl.
HEALTHCHECK --interval=10s --timeout=5s --start-period=20s --retries=6 \
    CMD ["/vantage-control-api", "healthcheck"]

ENTRYPOINT ["/vantage-control-api"]
CMD ["serve"]
