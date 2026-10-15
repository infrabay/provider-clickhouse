# Test-only image of the fake GCE metadata server and Google JWKS
# (test/fakegcp). Never deploy it next to real workloads: it mints tokens for
# any identity. Build from the repository root:
#
#   docker build -f test/fakegcp/Dockerfile -t fakegcp:dev .
#
# The package uses the standard library only, so the build needs no module
# downloads.
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
COPY test/fakegcp/ test/fakegcp/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local \
    go build -trimpath -ldflags "-s -w" -o /out/fakegcp ./test/fakegcp/cmd/fakegcp

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

COPY --from=build /out/fakegcp /usr/local/bin/fakegcp

USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/fakegcp"]
