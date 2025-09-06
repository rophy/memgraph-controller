const neo4j = require('neo4j-driver');

class MetricsTracker {
    constructor() {
        this.reset();
    }

    reset() {
        this.totalCount = 0;
        this.successCount = 0;
        this.errorCount = 0;
        this.latencies = [];
        this.startTime = Date.now();
    }

    recordSuccess(latencyMs) {
        this.totalCount++;
        this.successCount++;
        this.latencies.push(latencyMs);
    }

    recordError() {
        this.totalCount++;
        this.errorCount++;
    }

    getStats() {
        const sortedLatencies = [...this.latencies].sort((a, b) => a - b);
        const len = sortedLatencies.length;
        
        return {
            total: this.totalCount,
            success: this.successCount,
            errors: this.errorCount,
            errorRate: this.totalCount > 0 ? (this.errorCount / this.totalCount * 100).toFixed(2) + '%' : '0%',
            uptime: Math.floor((Date.now() - this.startTime) / 1000) + 's',
            latency: len > 0 ? {
                min: Math.min(...sortedLatencies),
                max: Math.max(...sortedLatencies),
                avg: Math.floor(sortedLatencies.reduce((a, b) => a + b, 0) / len),
                median: len % 2 === 0 
                    ? Math.floor((sortedLatencies[len/2-1] + sortedLatencies[len/2]) / 2)
                    : sortedLatencies[Math.floor(len/2)],
                p95: sortedLatencies[Math.floor(len * 0.95)] || sortedLatencies[len - 1],
                p99: sortedLatencies[Math.floor(len * 0.99)] || sortedLatencies[len - 1]
            } : null
        };
    }
}

class Neo4jClient {
    constructor(uri, username = '', password = '') {
        console.log(`Initializing Neo4j client for ${uri}`);
        
        const authConfig = username && password 
            ? neo4j.auth.basic(username, password)
            : undefined;
            
        this.driver = neo4j.driver(uri, authConfig, {
            maxConnectionPoolSize: 10,
            connectionAcquisitionTimeout: 30000,
            maxTransactionRetryTime: 30000,
            logging: {
                level: 'info',
                logger: (level, message) => {
                    if (level === 'error') {
                        console.error(`[Neo4j] ${message}`);
                        // Count driver-level errors in metrics
                        this.metrics.recordError();
                    }
                }
            }
        });
        
        this.metrics = new MetricsTracker();
        this.writeInterval = parseInt(process.env.WRITE_INTERVAL || '1000');
        this.running = false;
        this.paused = false;
    }

    async verifyConnection() {
        try {
            await this.driver.verifyConnectivity();
            console.log('✓ Successfully connected to Neo4j');
            return true;
        } catch (error) {
            this.metrics.recordError();
            console.error(`✗ Connection failed: ${error.message} | Total: ${this.metrics.totalCount}, Success: ${this.metrics.successCount}, Errors: ${this.metrics.errorCount}`);
            return false;
        }
    }

    async writeData() {
        const startTime = Date.now();
        
        try {
            const timestamp = new Date().toISOString();
            const id = `client_${Date.now()}_${Math.random().toString(36).substr(2, 9)}`;
            
            const query = `
                CREATE (n:ClientData {
                    id: $id,
                    timestamp: $timestamp,
                    value: $value,
                    created_at: datetime()
                })
                RETURN n.id as id
            `;
            
            const result = await this.driver.executeQuery(query, {
                id: id,
                timestamp: timestamp,
                value: `data_${Date.now()}`
            });

            const latency = Date.now() - startTime;
            this.metrics.recordSuccess(latency);
            
            console.log(`✓ Success | Total: ${this.metrics.totalCount}, Success: ${this.metrics.successCount}, Errors: ${this.metrics.errorCount}`);
            
            return true;
        } catch (error) {
            this.metrics.recordError();
            console.error(`✗ Failed | Total: ${this.metrics.totalCount}, Success: ${this.metrics.successCount}, Errors: ${this.metrics.errorCount} | Error: ${error.message}`);
            return false;
        }
    }

    async start() {
        console.log('Starting Neo4j client...');
        console.log(`Configuration:`);
        console.log(`  - Server: ${process.env.NEO4J_URI || 'bolt://localhost:7687'}`);
        console.log(`  - Write Interval: ${this.writeInterval}ms`);
        console.log(`  - Username: ${process.env.NEO4J_USERNAME || '(none)'}`);
        
        const connected = await this.verifyConnection();
        if (!connected) {
            console.error('Cannot start without connection. Retrying in 5 seconds...');
            setTimeout(() => this.start(), 5000);
            return;
        }

        this.running = true;
        console.log('\n=== Starting write loop ===\n');
        
        while (this.running) {
            if (!this.paused) {
                await this.writeData();
            } else {
                console.log('⏸️  Client paused - waiting for resume signal');
            }
            await new Promise(resolve => setTimeout(resolve, this.writeInterval));
        }
    }

    pause() {
        console.log('\n⏸️  === Pausing client ===');
        this.paused = true;
    }

    resume() {
        console.log('\n▶️  === Resuming client ===');
        this.paused = false;
    }

    async stop() {
        console.log('\n=== Stopping client ===');
        this.running = false;
        
        const finalStats = this.metrics.getStats();
        console.log(`📊 Final Stats | Total: ${finalStats.total}, Success: ${finalStats.success}, Errors: ${finalStats.errors} (${finalStats.errorRate}), Uptime: ${finalStats.uptime}`);
        
        await this.driver.close();
        console.log('✓ Client stopped');
    }
}

// One-shot query mode
async function runOneShot(query) {
    const uri = process.env.NEO4J_URI || 'bolt://localhost:7687';
    const username = process.env.NEO4J_USERNAME || '';
    const password = process.env.NEO4J_PASSWORD || '';
    
    const authConfig = username && password 
        ? neo4j.auth.basic(username, password)
        : undefined;
        
    const driver = neo4j.driver(uri, authConfig, {
        maxConnectionPoolSize: 1,
        connectionAcquisitionTimeout: 10000,
        maxTransactionRetryTime: 10000
    });
    
    try {
        // Use session with auto-commit for replication commands
        const session = driver.session();
        const result = await session.run(query);
        
        // Convert result to simplified format
        const records = result.records.map(record => {
            return record.toObject();
        });
        
        // Output records as JSON to stdout
        console.log(JSON.stringify(records));
        
        await session.close();
        await driver.close();
        process.exit(0);
    } catch (error) {
        // Output error to stderr
        console.error(error);
        await driver.close();
        process.exit(1);
    }
}

async function main() {
    // Check if command line args are provided for one-shot mode
    const args = process.argv.slice(2);
    if (args.length > 0) {
        // One-shot mode: join all args as a single query
        const query = args.join(' ');
        await runOneShot(query);
        return;
    }
    
    // Continuous mode (existing behavior)
    const uri = process.env.NEO4J_URI || 'bolt://localhost:7687';
    const username = process.env.NEO4J_USERNAME || 'memgraph';
    const password = process.env.NEO4J_PASSWORD || '';
    
    const client = new Neo4jClient(uri, username, password);
    
    // Graceful shutdown
    const shutdown = async (signal) => {
        console.log(`\nReceived ${signal}, shutting down gracefully...`);
        await client.stop();
        process.exit(0);
    };
    
    // Pause/Resume handlers
    const pauseHandler = (signal) => {
        console.log(`\nReceived ${signal}, pausing client...`);
        client.pause();
    };
    
    const resumeHandler = (signal) => {
        console.log(`\nReceived ${signal}, resuming client...`);
        client.resume();
    };
    
    process.on('SIGTERM', () => shutdown('SIGTERM'));
    process.on('SIGINT', () => shutdown('SIGINT'));
    process.on('SIGUSR1', () => pauseHandler('SIGUSR1')); // Pause signal
    process.on('SIGUSR2', () => resumeHandler('SIGUSR2')); // Resume signal
    
    // Start the client
    await client.start();
}

// Run the application
main().catch(error => {
    console.error('Fatal error:', error);
    process.exit(1);
});