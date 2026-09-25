# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/stalker ./cmd/stalker
# distroless has no shell, so the data directory is prepared here
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/stalker /stalker
# a new named volume copies this directory, including its nonroot ownership
COPY --from=build --chown=65532:65532 /out/data /data
ENV STALKER_DB=/data/stalker.db \
    STALKER_ADDR=:8080
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
# no HEALTHCHECK: distroless has no shell or curl; monitor GET /healthz from outside instead
ENTRYPOINT ["/stalker"]
CMD ["serve"]
