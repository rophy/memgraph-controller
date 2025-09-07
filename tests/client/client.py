#!/usr/bin/env python3
"""
Python Neo4j test client with consistent logfmt output
Equivalent functionality to the Node.js version but with structured logging
"""

import os
import sys
import time
import json
import signal
import asyncio
from datetime import datetime
from typing import Optional, Dict, Any, List
import statistics

from neo4j import GraphDatabase, Driver
import logging
from logfmter import Logfmter

# Initialize logfmt logger
logger = logging.getLogger('neo4j-client')
handler = logging.StreamHandler()
formatter = Logfmter()
handler.setFormatter(formatter)
logger.addHandler(handler)
logger.setLevel(logging.INFO)


class MetricsTracker:
    """Track client metrics for performance analysis"""
    
    def __init__(self):
        self.reset()
    
    def reset(self):
        self.total_count = 0
        self.success_count = 0
        self.error_count = 0
        self.latencies = []
        self.start_time = time.time()
    
    def record_success(self, latency_ms: float):
        self.total_count += 1
        self.success_count += 1
        self.latencies.append(latency_ms)
    
    def record_error(self):
        self.total_count += 1
        self.error_count += 1
    
    def get_stats(self) -> Dict[str, Any]:
        if not self.latencies:
            latency_stats = None
        else:
            sorted_latencies = sorted(self.latencies)
            len_latencies = len(sorted_latencies)
            
            latency_stats = {
                'min': min(sorted_latencies),
                'max': max(sorted_latencies),
                'avg': int(statistics.mean(sorted_latencies)),
                'median': statistics.median(sorted_latencies),
                'p95': sorted_latencies[int(len_latencies * 0.95)] if len_latencies > 0 else 0,
                'p99': sorted_latencies[int(len_latencies * 0.99)] if len_latencies > 0 else 0
            }
        
        error_rate = f"{(self.error_count / self.total_count * 100):.2f}%" if self.total_count > 0 else "0%"
        uptime = f"{int(time.time() - self.start_time)}s"
        
        return {
            'total': self.total_count,
            'success': self.success_count,
            'errors': self.error_count,
            'error_rate': error_rate,
            'uptime': uptime,
            'latency': latency_stats
        }


class Neo4jClient:
    """Neo4j client with metrics tracking and logfmt output"""
    
    def __init__(self, uri: str, username: str = '', password: str = ''):
        logger.info("Initializing Neo4j client", extra={"uri": uri})
        
        auth = (username, password) if username and password else None
        
        self.driver = GraphDatabase.driver(
            uri, 
            auth=auth,
            max_connection_pool_size=10,
            connection_acquisition_timeout=30,
            max_transaction_retry_time=30
        )
        
        self.metrics = MetricsTracker()
        self.write_interval = int(os.getenv('WRITE_INTERVAL', '1000')) / 1000.0  # Convert to seconds
        self.running = False
        self.paused = False
    
    def verify_connection(self) -> bool:
        """Verify connection to Neo4j database"""
        try:
            self.driver.verify_connectivity()
            logger.info("Successfully connected to Neo4j")
            return True
        except Exception as error:
            self.metrics.record_error()
            logger.error("Connection failed", extra={
                "error": str(error),
                "total": self.metrics.total_count,
                "success": self.metrics.success_count,
                "errors": self.metrics.error_count
            })
            return False
    
    def write_data(self) -> bool:
        """Write test data to the database"""
        start_time = time.time()
        
        try:
            timestamp = datetime.now().isoformat()
            node_id = f"client_{int(time.time() * 1000)}_{hex(int(time.time() * 1000000))[-8:]}"
            
            query = """
                CREATE (n:ClientData {
                    id: $id,
                    timestamp: $timestamp,
                    value: $value,
                    created_at: datetime()
                })
                RETURN n.id as id
            """
            
            with self.driver.session() as session:
                result = session.run(query, {
                    'id': node_id,
                    'timestamp': timestamp,
                    'value': f"data_{int(time.time() * 1000)}"
                })
                
                # Consume result to ensure query execution
                list(result)
            
            latency = (time.time() - start_time) * 1000  # Convert to milliseconds
            self.metrics.record_success(latency)
            
            logger.info("Success", extra={
                "total": self.metrics.total_count,
                "success": self.metrics.success_count,
                "errors": self.metrics.error_count
            })
            
            return True
            
        except Exception as error:
            self.metrics.record_error()
            logger.error("Failed", extra={
                "total": self.metrics.total_count,
                "success": self.metrics.success_count,
                "errors": self.metrics.error_count,
                "error": str(error)
            })
            return False
    
    async def start(self):
        """Start the client write loop"""
        logger.info("Starting Neo4j client", extra={
            "server": os.getenv('NEO4J_URI', 'bolt://localhost:7687'),
            "write_interval": int(self.write_interval * 1000),  # Log in ms
            "username": os.getenv('NEO4J_USERNAME', '(none)')
        })
        
        connected = self.verify_connection()
        if not connected:
            logger.error("Cannot start without connection. Retrying in 5 seconds...")
            await asyncio.sleep(5)
            return await self.start()
        
        self.running = True
        logger.info("Starting write loop")
        
        while self.running:
            if not self.paused:
                self.write_data()
            else:
                logger.info("Client paused - waiting for resume signal")
            
            await asyncio.sleep(self.write_interval)
    
    def pause(self):
        """Pause the client write loop"""
        logger.info("Pausing client")
        self.paused = True
    
    def resume(self):
        """Resume the client write loop"""
        logger.info("Resuming client")
        self.paused = False
    
    def stop(self):
        """Stop the client and log final statistics"""
        logger.info("Stopping client")
        self.running = False
        
        final_stats = self.metrics.get_stats()
        logger.info("Final Stats", extra={
            "total": final_stats['total'],
            "success": final_stats['success'],
            "errors": final_stats['errors'],
            "error_rate": final_stats['error_rate'],
            "uptime": final_stats['uptime']
        })
        
        self.driver.close()
        logger.info("Client stopped")


def run_one_shot(query: str):
    """Execute a single query and output results as JSON"""
    uri = os.getenv('NEO4J_URI', 'bolt://localhost:7687')
    username = os.getenv('NEO4J_USERNAME', '')
    password = os.getenv('NEO4J_PASSWORD', '')
    
    auth = (username, password) if username and password else None
    driver = GraphDatabase.driver(
        uri,
        auth=auth,
        max_connection_pool_size=1,
        connection_acquisition_timeout=10,
        max_transaction_retry_time=10
    )
    
    try:
        with driver.session() as session:
            result = session.run(query)
            
            # Convert result to simplified format
            records = []
            for record in result:
                record_dict = dict(record)
                records.append(record_dict)
            
            # Output records as JSON to stdout
            print(json.dumps(records))
        
        driver.close()
        sys.exit(0)
        
    except Exception as error:
        print(f"Error: {error}", file=sys.stderr)
        driver.close()
        sys.exit(1)


async def main():
    """Main entry point"""
    # Check if command line args are provided for one-shot mode
    args = sys.argv[1:]
    if args:
        # One-shot mode: join all args as a single query
        query = ' '.join(args)
        run_one_shot(query)
        return
    
    # Continuous mode (existing behavior)
    uri = os.getenv('NEO4J_URI', 'bolt://localhost:7687')
    username = os.getenv('NEO4J_USERNAME', 'memgraph')
    password = os.getenv('NEO4J_PASSWORD', '')
    
    client = Neo4jClient(uri, username, password)
    
    # Graceful shutdown handlers
    def shutdown_handler(signum, frame):
        logger.info(f"Received signal {signum}, shutting down gracefully...")
        client.stop()
        sys.exit(0)
    
    def pause_handler(signum, frame):
        logger.info(f"Received signal {signum}, pausing client...")
        client.pause()
    
    def resume_handler(signum, frame):
        logger.info(f"Received signal {signum}, resuming client...")
        client.resume()
    
    # Register signal handlers
    signal.signal(signal.SIGTERM, shutdown_handler)
    signal.signal(signal.SIGINT, shutdown_handler)
    signal.signal(signal.SIGUSR1, pause_handler)  # Pause signal
    signal.signal(signal.SIGUSR2, resume_handler)  # Resume signal
    
    # Start the client
    try:
        await client.start()
    except KeyboardInterrupt:
        logger.info("Received interrupt, shutting down...")
        client.stop()


if __name__ == '__main__':
    asyncio.run(main())