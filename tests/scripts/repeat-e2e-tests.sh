#!/bin/bash

# Script to repeatedly run E2E tests and capture detailed logs
# Usage: ./tests/scripts/repeat-e2e-tests.sh <number_of_runs>

set -euo pipefail

# Check if number of runs is provided
if [ $# -ne 1 ]; then
    echo "Usage: $0 <number_of_runs>"
    echo "Example: $0 20"
    exit 1
fi

NUM_RUNS=$1

# Validate input is a positive integer
if ! [[ "$NUM_RUNS" =~ ^[1-9][0-9]*$ ]]; then
    echo "Error: Number of runs must be a positive integer"
    exit 1
fi

# Create logs directory if it doesn't exist
mkdir -p logs

echo "🔍 Running $NUM_RUNS E2E test iterations..."
echo "📁 Logs will be saved to logs/test_run_N.log"
echo ""

PASSED=0
FAILED=0

for i in $(seq 1 $NUM_RUNS); do
    LOG_FILE="logs/test_run_$i.log"
    
    echo "=== E2E Test Run $i/$NUM_RUNS ==="
    echo "📄 Logging to: $LOG_FILE"
    
    # Write header to log file
    {
        echo "=============================================="
        echo "E2E Test Run $i/$NUM_RUNS"
        echo "Timestamp: $(date)"
        echo "=============================================="
        echo ""
    } > "$LOG_FILE"
    
    # Run E2E test and capture all output
    if make test-e2e >> "$LOG_FILE" 2>&1; then
        echo "✅ Run $i: PASSED"
        PASSED=$((PASSED + 1))
        
        # Append cluster health check
        {
            echo ""
            echo "=============================================="
            echo "Post-Test Cluster Health Check"
            echo "=============================================="
        } >> "$LOG_FILE"
        
        make check >> "$LOG_FILE" 2>&1
        
    else
        echo "❌ Run $i: FAILED"
        FAILED=$((FAILED + 1))
        
        # Still try to get cluster health check for failed test
        {
            echo ""
            echo "=============================================="
            echo "Post-Failure Cluster Health Check"
            echo "=============================================="
        } >> "$LOG_FILE"
        
        make check >> "$LOG_FILE" 2>&1 || echo "Cluster health check also failed" >> "$LOG_FILE"
        
        echo ""
        echo "💥 Test failed on run $i/$NUM_RUNS"
        echo "📄 Check $LOG_FILE for detailed failure information"
        echo ""
        echo "📊 Summary: $PASSED passed, $FAILED failed out of $i total runs"
        exit 1
    fi
    
    echo ""
done

echo "🎉 All $NUM_RUNS E2E test runs completed successfully!"
echo "📊 Final Summary: $PASSED passed, $FAILED failed"
echo "📁 All logs saved to logs/ directory"