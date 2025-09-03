# Build stage
FROM golang:1.24-alpine AS builder

# Set working directory
WORKDIR /app

# Copy go.mod and go.sum from project root
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy test source code
COPY tests/*.go ./

# Build test binary
RUN CGO_ENABLED=0 GOOS=linux go test -c -o e2e-tests .

# Final stage
FROM alpine:3.18

# Install CA certificates and kubectl for HTTPS connections and K8s operations
RUN apk add --no-cache ca-certificates curl && \
    curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" && \
    chmod +x kubectl && \
    mv kubectl /usr/local/bin/

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