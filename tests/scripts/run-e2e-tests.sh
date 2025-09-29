#!/bin/bash
set -euo pipefail

# Python E2E Test Wrapper Script
# Checks prerequisites and runs Python-based e2e tests


# Colors for output
readonly GREEN='\033[0;32m'
readonly RED='\033[0;31m'
readonly BLUE='\033[0;34m'
readonly YELLOW='\033[1;33m'
readonly NC='\033[0m'


log_info() { 
    echo -e "${BLUE}[INFO]${NC} $(date -Iseconds) $*"; 
}

log_success() { 
    echo -e "${GREEN}[SUCCESS]${NC} $(date -Iseconds) $*"; 
}

log_error() { 
    echo -e "${RED}[ERROR]${NC} $(date -Iseconds) $*" >&2; 
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $(date -Iseconds) $*";
}

check_prerequisites() {
    log_info "🔍 Checking prerequisites for Python E2E tests..."
    
    # Check if python is available
    if ! command -v python &> /dev/null; then
        log_error "python is required but not installed"
        log_error "Please install Python 3.8+ to run Python E2E tests"
        exit 1
    fi
    
    local python_version
    python_version=$(python --version 2>&1 | cut -d' ' -f2)
    log_info "✅ Found Python: $python_version"
    
    # Check kubectl
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl is required but not installed"
        exit 1
    fi
    
    # Check kubernetes connectivity
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Cannot connect to Kubernetes cluster"
        log_error "Please ensure kubectl is configured and cluster is accessible"
        exit 1
    fi
    
    # Check if memgraph namespace exists
    if ! kubectl get namespace memgraph &> /dev/null; then
        log_error "Namespace 'memgraph' does not exist"
        log_error "Please deploy memgraph cluster first"
        exit 1
    fi
    
    log_success "✅ All prerequisites satisfied"
}

setup_python_environment() {
    log_info "🐍 Setting up Python test environment..."

    local test_dir="tests/e2e"
    if [ ! -d "$test_dir" ]; then
        log_error "Python test directory '$test_dir' not found"
        exit 1
    fi

    if [ ! -f "$test_dir/requirements.txt" ]; then
        log_error "requirements.txt not found in '$test_dir'"
        exit 1
    fi

    # Install/upgrade requirements
    log_info "📦 Installing Python dependencies..."
    cd "$test_dir"

    cd - > /dev/null
}

get_leader_controller_pod() {
    log_info "🔍 Identifying leader controller pod..." >&2

    # Get controller pods with Ready status
    local leader_pod
    leader_pod=$(kubectl get pods -n memgraph -l app=memgraph-controller -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep "True" | head -1 | cut -d' ' -f1)

    if [ -z "$leader_pod" ]; then
        log_warning "⚠️  No ready controller pod found" >&2
        return 1
    fi

    log_info "✅ Leader controller pod: $leader_pod" >&2
    echo "$leader_pod"
}

dump_pod_logs() {
    local since_time="$1"

    log_info "📋 Dumping all pod logs since test start..."

    # Generate timestamp for log directory
    local timestamp
    timestamp=$(date +"%Y%m%d_%H%M%S")
    local log_dir="logs/${timestamp}"

    # Create timestamped logs directory
    mkdir -p "$log_dir"

    # Get leader controller pod and echo it clearly
    local leader_pod
    if leader_pod=$(get_leader_controller_pod); then
        echo "🎯 Leader controller pod: $leader_pod"
    else
        log_warning "⚠️  No leader controller found"
    fi

    # Get all pods in memgraph namespace
    local all_pods
    all_pods=$(kubectl get pods -n memgraph -o jsonpath='{.items[*].metadata.name}')

    if [ -z "$all_pods" ]; then
        log_warning "⚠️  No pods found in memgraph namespace"
        return 0
    fi

    log_info "📝 Dumping logs from all pods since $since_time to $log_dir/"

    local success_count=0
    local total_count=0

    for pod in $all_pods; do
        total_count=$((total_count + 1))
        local log_file="$log_dir/${pod}.log"

        # Dump logs with timestamps, filtering from test start time
        local kubectl_cmd
        if [ -n "$since_time" ]; then
            kubectl_cmd="kubectl logs -n memgraph $pod --timestamps --since-time=$since_time"
        else
            kubectl_cmd="kubectl logs -n memgraph $pod --timestamps"
        fi

        if eval "$kubectl_cmd" > "$log_file" 2>/dev/null; then
            # Show log file size and line count for reference
            local lines size
            lines=$(wc -l < "$log_file")
            size=$(du -h "$log_file" | cut -f1)

            if [ "$lines" -eq 0 ]; then
                log_info "📊 $pod: empty log (no logs since test start)"
            else
                log_info "📊 $pod: $lines lines ($size) saved to ${pod}.log"
            fi
            success_count=$((success_count + 1))
        else
            log_warning "⚠️  Failed to dump logs for pod: $pod"
        fi
    done

    if [ "$success_count" -eq "$total_count" ]; then
        log_success "✅ All pod logs ($success_count/$total_count) saved to $log_dir/"
    else
        log_warning "⚠️  Partial success: $success_count/$total_count pod logs saved to $log_dir/"
    fi
}

run_python_tests() {
    log_info "🚀 Running Python E2E Tests"
    log_info "=" * 50

    local test_dir="tests/e2e"
    local test_result=0

    # Capture test start time in RFC3339 format for kubectl logs --since-time
    local test_start_time
    test_start_time=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    log_info "📅 Test started at: $test_start_time"

    # Run pytest with verbose output and short traceback
    # Using -x to stop on first failure for faster feedback
    # -s shows stdout/stderr from tests
    local pytest_args="-v -s -x --tb=short"

    # Add specific test selection if provided as argument
    if [ $# -gt 0 ]; then
        pytest_args="$pytest_args $*"
    fi

    log_info "Running: python -m pytest $pytest_args $test_dir"

    if python -u -m pytest $pytest_args "$test_dir"; then
        log_success "🎉 All Python E2E tests passed!"
        test_result=0
    else
        log_error "💥 Some Python E2E tests failed!"
        test_result=1
    fi

    # Always dump pod logs after tests finish (regardless of test result)
    echo
    dump_pod_logs "$test_start_time"

    return $test_result
}

main() {
    echo
    log_info "🐍 Starting Python-based Memgraph E2E Tests"
    log_info "============================================"
    echo
    
    # Check prerequisites first
    venv_dir=$(dirname "$0")/../../venv
    if [ ! -d "$venv_dir" ]; then
        log_error "Virtual environment not found at $venv_dir"
        log_error "Please run 'tests/scripts/init-e2e-tests.sh' to create it"
        exit 1
    fi
    source $venv_dir/bin/activate
    
    check_prerequisites
    echo
    
    # Setup Python environment
    setup_python_environment
    echo
    
    # Run the tests
    run_python_tests "$@"
}

# Run main function with all arguments
main "$@"
