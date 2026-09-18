FROM golang:1.24-alpine AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.mod
COPY go.sum go.sum

# Cache dependencies
RUN go mod download

# Copy source code
COPY cmd/ cmd/
COPY internal/ internal/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o webhook ./cmd/

# Use distroless as minimal runtime image
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /workspace/webhook .

USER 65532:65532

ENTRYPOINT ["/webhook"]
