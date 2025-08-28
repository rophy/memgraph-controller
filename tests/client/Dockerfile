FROM node:20-alpine

# Set working directory
WORKDIR /app

# Copy package files
COPY package.json ./

# Install dependencies
RUN npm ci --only=production

# Copy application code
COPY index.js ./

# Create non-root user
RUN addgroup -g 1001 -S nodejs && \
    adduser -S nodejs -u 1001

# Change ownership
RUN chown -R nodejs:nodejs /app

# Switch to non-root user
USER nodejs

# Set default environment variables
ENV NEO4J_URI=bolt://localhost:7687 \
    NEO4J_USERNAME="" \
    NEO4J_PASSWORD="" \
    WRITE_INTERVAL=1000

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD node -e "process.exit(0)" || exit 1

# Run the application
CMD ["node", "index.js"]