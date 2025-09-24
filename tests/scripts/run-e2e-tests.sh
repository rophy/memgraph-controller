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

run_python_tests() {
    log_info "🚀 Running Python E2E Tests"
    log_info "=" * 50
    
    local test_dir="tests/e2e"
    
    # Run pytest with verbose output and short traceback
    # Using -x to stop on first failure for faster feedback
    # -s shows stdout/stderr from tests
    local pytest_args="-v -s -x --tb=short"
    
    # Add specific test selection if provided as argument
    if [ $# -gt 0 ]; then
        pytest_args="$pytest_args $*"
    fi
    
    log_info "Running: python -m pytest $pytest_args $test_dir"
    
    if python -m pytest $pytest_args "$test_dir"; then
        log_success "🎉 All Python E2E tests passed!"
        return 0
    else
        log_error "💥 Some Python E2E tests failed!"
        return 1
    fi
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
