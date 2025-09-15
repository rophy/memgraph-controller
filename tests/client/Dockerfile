FROM python:3.12-alpine

# Set working directory
WORKDIR /app

# Install system utilities for debugging
RUN apk add --no-cache bash curl jq

# Copy requirements and install dependencies
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

# Copy application code
COPY client.py ./

# Create non-root user
RUN addgroup -g 1001 -S python && \
    adduser -S python -u 1001

# Change ownership
RUN chown -R python:python /app

# Switch to non-root user
USER python

# Set default environment variables
ENV NEO4J_URI=bolt://localhost:7687 \
    NEO4J_USERNAME="" \
    NEO4J_PASSWORD="" \
    WRITE_INTERVAL=1000

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD python -c "import sys; sys.exit(0)" || exit 1

# Entry point for the application
ENTRYPOINT ["python", "/app/client.py"]

# Default command for e2e tests - write to controller
CMD ["write", "bolt://memgraph-controller:7687"]