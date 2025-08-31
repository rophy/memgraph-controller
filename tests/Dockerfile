# Build stage
FROM golang:1.21-alpine AS builder

# Set working directory
WORKDIR /app

# Copy go.mod and go.sum from tests directory
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy test source code
COPY *.go ./

# Build test binary
RUN CGO_ENABLED=0 GOOS=linux go test -c -o e2e-tests ./...

# Final stage
FROM alpine:3.18

# Install CA certificates for HTTPS connections
RUN apk add --no-cache ca-certificates

# Create non-root user
RUN adduser -D -s /bin/sh testuser

# Copy test binary from builder
COPY --from=builder /app/e2e-tests /usr/local/bin/e2e-tests

# Set ownership
RUN chown testuser:testuser /usr/local/bin/e2e-tests

# Switch to non-root user
USER testuser

# Run tests by default
CMD ["/usr/local/bin/e2e-tests", "-test.v"]