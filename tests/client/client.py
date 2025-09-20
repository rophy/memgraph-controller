#!/usr/bin/env python3
"""
Enhanced Memgraph test client with command-driven architecture
Supports: write, query, logs, and idle modes
"""

import os
import sys
import time
import json
import signal
import asyncio
from datetime import datetime
from typing import Dict, Any
import statistics

from neo4j import GraphDatabase
import logging
from logfmter import Logfmter

try:
    import websockets
except ImportError:
    websockets = None
    print("Warning: websockets library not installed. 'logs' command will not work.", file=sys.stderr)


class LogFormatter(Logfmter):
    def format(self, record):
        record.ts = datetime.now().astimezone().isoformat()
        return super().format(record)


# Initialize logfmt logger with ts first, then level
logger = logging.getLogger('test-client')
handler = logging.StreamHandler()
formatter = LogFormatter(
    keys=["ts", "at"],
    mapping={"ts": "ts", "at": "levelname"}
)
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

    def __init__(self, bolt_address: str, username: str = '', password: str = ''):
        logger.info("Initializing Neo4j client", extra={"bolt_address": bolt_address})

        auth = (username, password) if username and password else None

        self.driver = GraphDatabase.driver(
            bolt_address,
            auth=auth,
            max_connection_pool_size=1,
            connection_acquisition_timeout=3,
            max_transaction_retry_time=2,
            connection_timeout=3
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

            logger.info("Write success", extra={
                "total": self.metrics.total_count,
                "success": self.metrics.success_count,
                "errors": self.metrics.error_count,
                "latency_ms": int(latency)
            })

            return True

        except Exception as error:
            self.metrics.record_error()
            logger.error("Write failed", extra={
                "total": self.metrics.total_count,
                "success": self.metrics.success_count,
                "errors": self.metrics.error_count,
                "error": str(error)
            })
            return False

    def read_data(self) -> bool:
        """Read test data from the database"""
        start_time = time.time()

        try:
            query = """
                MATCH (n:ClientData)
                RETURN count(n) as total_count,
                       min(n.created_at) as oldest_timestamp,
                       max(n.created_at) as newest_timestamp
            """

            with self.driver.session() as session:
                result = session.run(query)
                records = list(result)

                # Extract result data for logging
                if records:
                    record = records[0]
                    total_count = record["total_count"]
                else:
                    total_count = 0

            latency = (time.time() - start_time) * 1000  # Convert to milliseconds
            self.metrics.record_success(latency)

            logger.info("Read success", extra={
                "total": self.metrics.total_count,
                "success": self.metrics.success_count,
                "errors": self.metrics.error_count,
                "latency_ms": int(latency),
                "total_count": total_count
            })

            return True

        except Exception as error:
            self.metrics.record_error()
            logger.error("Read failed", extra={
                "total": self.metrics.total_count,
                "success": self.metrics.success_count,
                "errors": self.metrics.error_count,
                "error": str(error)
            })
            return False

    def execute_query(self, query: str) -> list:
        """Execute a single query and return results"""
        try:
            with self.driver.session() as session:
                result = session.run(query)
                records = []
                for record in result:
                    record_dict = dict(record)
                    # Convert datetime objects to strings for JSON serialization
                    for key, value in record_dict.items():
                        if hasattr(value, 'isoformat'):
                            record_dict[key] = value.isoformat()
                    records.append(record_dict)
                return records
        except Exception as error:
            logger.error("Query failed", extra={"error": str(error), "query": query})
            raise

    async def start_write_loop(self):
        """Start the continuous write loop"""
        connected = self.verify_connection()
        if not connected:
            logger.error("Cannot start without connection. Retrying in 5 seconds...")
            await asyncio.sleep(5)
            return await self.start_write_loop()

        self.running = True
        logger.info("Starting write loop", extra={"interval_ms": int(self.write_interval * 1000)})

        while self.running:
            if not self.paused:
                self.write_data()
            else:
                logger.info("Client paused - waiting for resume signal")

            await asyncio.sleep(self.write_interval)

    async def start_read_loop(self):
        """Start the continuous read loop"""
        connected = self.verify_connection()
        if not connected:
            logger.error("Cannot start without connection. Retrying in 5 seconds...")
            await asyncio.sleep(5)
            return await self.start_read_loop()

        self.running = True
        logger.info("Starting read loop", extra={"interval_ms": int(self.write_interval * 1000)})

        while self.running:
            if not self.paused:
                self.read_data()
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
        logger.info("Final stats", extra={
            "total": final_stats['total'],
            "success": final_stats['success'],
            "errors": final_stats['errors'],
            "error_rate": final_stats['error_rate'],
            "uptime": final_stats['uptime']
        })

        self.driver.close()
        logger.info("Client stopped")


class LogCollector:
    """WebSocket log collector for Memgraph logs"""

    def __init__(self, websocket_address: str, output_file: str):
        self.websocket_address = websocket_address
        self.output_file = output_file
        self.running = False

    async def collect_logs(self):
        """Connect to WebSocket and stream logs to file"""
        if websockets is None:
            logger.error("websockets library not installed. Install with: pip install websockets")
            return

        logger.info("Starting log collection", extra={
            "websocket": self.websocket_address,
            "output": self.output_file
        })

        self.running = True
        reconnect_delay = 5

        while self.running:
            try:
                async with websockets.connect(self.websocket_address) as websocket:
                    logger.info("Connected to WebSocket", extra={"address": self.websocket_address})
                    reconnect_delay = 5  # Reset delay on successful connection

                    with open(self.output_file, 'a') as f:
                        async for message in websocket:
                            if not self.running:
                                break

                            try:
                                # Parse the WebSocket message
                                data = json.loads(message)

                                # Add timestamp and save as JSONL
                                log_entry = {
                                    "timestamp": datetime.now().astimezone().isoformat(),
                                    "level": data.get("level", "UNKNOWN"),
                                    "message": data.get("message", ""),
                                    "raw": data
                                }

                                f.write(json.dumps(log_entry) + '\n')
                                f.flush()  # Ensure immediate write

                                # Also log to console for visibility
                                logger.debug("Log collected", extra={
                                    "level": log_entry["level"],
                                    "message": log_entry["message"][:100]  # Truncate long messages
                                })

                            except json.JSONDecodeError as e:
                                logger.warning("Failed to parse WebSocket message", extra={
                                    "error": str(e),
                                    "message": message[:100]
                                })

            except websockets.exceptions.ConnectionClosed as e:
                logger.warning("WebSocket connection closed", extra={"reason": str(e)})
                if self.running:
                    logger.info(f"Reconnecting in {reconnect_delay} seconds...")
                    await asyncio.sleep(reconnect_delay)
                    reconnect_delay = min(reconnect_delay * 2, 60)  # Exponential backoff

            except Exception as e:
                logger.error("WebSocket error", extra={"error": str(e)})
                if self.running:
                    logger.info(f"Reconnecting in {reconnect_delay} seconds...")
                    await asyncio.sleep(reconnect_delay)
                    reconnect_delay = min(reconnect_delay * 2, 60)

        logger.info("Log collection stopped")

    def stop(self):
        """Stop log collection"""
        self.running = False


async def run_idle_mode():
    """Run in idle mode - just sleep forever"""
    logger.info("Running in idle mode - sleeping forever")
    try:
        while True:
            await asyncio.sleep(3600)  # Sleep 1 hour at a time
    except KeyboardInterrupt:
        logger.info("Idle mode interrupted")


async def run_write_mode(bolt_address: str):
    """Run continuous write mode"""
    username = os.getenv('NEO4J_USERNAME', 'memgraph')
    password = os.getenv('NEO4J_PASSWORD', '')

    logger.info("Starting write mode", extra={"bolt_address": bolt_address})
    client = Neo4jClient(bolt_address, username, password)

    # Setup signal handlers
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

    signal.signal(signal.SIGTERM, shutdown_handler)
    signal.signal(signal.SIGINT, shutdown_handler)
    signal.signal(signal.SIGUSR1, pause_handler)
    signal.signal(signal.SIGUSR2, resume_handler)

    try:
        await client.start_write_loop()
    except KeyboardInterrupt:
        logger.info("Received interrupt, shutting down...")
        client.stop()


async def run_read_mode(bolt_address: str):
    """Run continuous read mode"""
    username = os.getenv('NEO4J_USERNAME', 'memgraph')
    password = os.getenv('NEO4J_PASSWORD', '')

    logger.info("Starting read mode", extra={"bolt_address": bolt_address})
    client = Neo4jClient(bolt_address, username, password)

    # Setup signal handlers
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

    signal.signal(signal.SIGTERM, shutdown_handler)
    signal.signal(signal.SIGINT, shutdown_handler)
    signal.signal(signal.SIGUSR1, pause_handler)
    signal.signal(signal.SIGUSR2, resume_handler)

    try:
        await client.start_read_loop()
    except KeyboardInterrupt:
        logger.info("Received interrupt, shutting down...")
        client.stop()


async def run_query(bolt_address: str, cypher_query: str):
    """Execute a single query"""
    username = os.getenv('NEO4J_USERNAME', 'memgraph')
    password = os.getenv('NEO4J_PASSWORD', '')

    logger.info("Executing query", extra={"bolt_address": bolt_address, "query": cypher_query})

    client = Neo4jClient(bolt_address, username, password)
    try:
        results = client.execute_query(cypher_query)
        # Output results as JSON to stdout
        print(json.dumps(results, indent=2))
        client.driver.close()
        return 0
    except Exception as e:
        logger.error("Query execution failed", extra={"error": str(e)})
        client.driver.close()
        return 1


async def run_log_collector(websocket_address: str, output_file: str):
    """Run WebSocket log collector"""
    collector = LogCollector(websocket_address, output_file)

    # Setup signal handler for graceful shutdown
    def shutdown_handler(signum, frame):
        logger.info(f"Received signal {signum}, stopping log collection...")
        collector.stop()

    signal.signal(signal.SIGTERM, shutdown_handler)
    signal.signal(signal.SIGINT, shutdown_handler)

    try:
        await collector.collect_logs()
    except KeyboardInterrupt:
        logger.info("Log collection interrupted")
        collector.stop()


def print_usage():
    """Print usage information"""
    print("""
Usage: client.py [command] [args...]

Commands:
  (no command)                              - Default: write to bolt://memgraph-gateway:7687
  idle                                      - Sleep forever (useful for sandbox)
  write <bolt_address>                     - Continuous write mode to specified address
  read <bolt_address>                      - Continuous read mode to specified address
  query <bolt_address> <cypher_query>      - Execute a single query
  logs <websocket_address> <output_file>   - Collect WebSocket logs to file

Examples:
  python client.py                                              # Default write mode (RW gateway)
  python client.py idle                                         # Idle mode
  python client.py write bolt://memgraph-gateway:7687          # Write to RW gateway
  python client.py read bolt://memgraph-gateway-read:7688      # Read from RO gateway
  python client.py write bolt://memgraph-ha-0:7687             # Write to specific pod
  python client.py read bolt://memgraph-ha-1:7687              # Read from specific replica
  python client.py query bolt://memgraph-ha-0:7687 "SHOW REPLICATION ROLE"
  python client.py logs ws://10.244.0.5:7444 /tmp/logs.jsonl   # Collect logs

Environment Variables:
  NEO4J_USERNAME    - Username for authentication (default: memgraph)
  NEO4J_PASSWORD    - Password for authentication (default: empty)
  WRITE_INTERVAL    - Write interval in milliseconds (default: 1000)
""")


async def main():
    """Main entry point"""
    args = sys.argv[1:]

    # Default behavior: show help and exit.
    if not args:
        print_usage()
        return 0

    command = args[0]

    if command in ['-h', '--help', 'help']:
        print_usage()
        return 0

    elif command == "idle":
        return await run_idle_mode()

    elif command == "write":
        if len(args) < 2:
            print("Error: write command requires bolt_address", file=sys.stderr)
            print("Usage: client.py write <bolt_address>", file=sys.stderr)
            return 1
        bolt_address = args[1]
        return await run_write_mode(bolt_address)

    elif command == "read":
        if len(args) < 2:
            print("Error: read command requires bolt_address", file=sys.stderr)
            print("Usage: client.py read <bolt_address>", file=sys.stderr)
            return 1
        bolt_address = args[1]
        return await run_read_mode(bolt_address)

    elif command == "query":
        if len(args) < 3:
            print("Error: query command requires bolt_address and cypher_query", file=sys.stderr)
            print("Usage: client.py query <bolt_address> <cypher_query>", file=sys.stderr)
            return 1
        bolt_address = args[1]
        cypher_query = args[2]
        return await run_query(bolt_address, cypher_query)

    elif command == "logs":
        if len(args) < 3:
            print("Error: logs command requires websocket_address and output_file", file=sys.stderr)
            print("Usage: client.py logs <websocket_address> <output_file>", file=sys.stderr)
            return 1
        websocket_address = args[1]
        output_file = args[2]
        return await run_log_collector(websocket_address, output_file)

    else:
        print(f"Error: Unknown command '{command}'", file=sys.stderr)
        print_usage()
        return 1


if __name__ == '__main__':
    sys.exit(asyncio.run(main()))
