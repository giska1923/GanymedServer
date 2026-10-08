# Build stage: the full Go toolchain. Module downloads are a separate layer, so editing code does
# not re-download dependencies.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 produces a fully static binary, which is what lets the runtime stage have no libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backend ./cmd/backend

# Runtime stage: distroless static has no shell and no package manager, just CA certificates,
# tzdata and a non-root user. Nothing to exploit, and ~2 MB beside the binary.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/backend /backend
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/backend"]
