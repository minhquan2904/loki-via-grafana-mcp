# Multi-stage build: the Go toolchain compiles, the runtime image holds the
# static binary and nothing else.
FROM golang:1.25 AS build

ARG VERSION=dev
WORKDIR /src

# Cache the module download before the sources change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Trimpath keeps the build reproducible; -s -w drops the symbol table and DWARF.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/server ./cmd/server

# distroless/static has no shell, no package manager and no curl, which is why
# the healthcheck is the binary itself.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server

USER nonroot:nonroot

ENV MCP_TRANSPORT=http \
    MCP_LISTEN=0.0.0.0:8000

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/server", "--healthcheck"]

ENTRYPOINT ["/server"]
