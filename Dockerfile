# Build stage
FROM golang:1.24-alpine AS builder

# Set working directory
WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY internal/ ./internal/
COPY cmd/ ./cmd/

# Build the controller binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -o bin/memgraph-controller ./cmd/memgraph-controller

# Final stage
FROM alpine:3.18

# Install ca-certificates for HTTPS connections
RUN apk --no-cache add ca-certificates

# Create non-root user
RUN adduser -D -s /bin/sh controller

WORKDIR /

# Copy the binary from builder stage
COPY --from=builder /workspace/bin/memgraph-controller .

# Change ownership to non-root user
RUN chown controller:controller /memgraph-controller

# Switch to non-root user
USER controller

# Set entrypoint
ENTRYPOINT ["/memgraph-controller"]