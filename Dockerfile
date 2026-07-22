# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS builder
WORKDIR /src

# Static binary, required by the distroless runtime stage below.
ENV CGO_ENABLED=0 GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG GIT_SHA=unknown
ARG BUILD_TIME=unknown
RUN LDFLAGS="-s -w -X main.version=${VERSION} -X main.gitSHA=${GIT_SHA} -X main.buildTime=${BUILD_TIME}" && \
    go build -ldflags "${LDFLAGS}" -o /out/server  ./cmd/api && \
    go build -ldflags "${LDFLAGS}" -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=builder /out/server  /app/server
COPY --from=builder /out/migrate /app/migrate
USER nonroot:nonroot
EXPOSE 8080
# Distroless has no shell/curl/wget, so the binary probes itself.
HEALTHCHECK --interval=5s --timeout=3s --retries=5 --start-period=5s \
    CMD ["/app/server", "-healthcheck"]
ENTRYPOINT ["/app/server"]
