#!/bin/bash

# Initialize E2E test environment
# Sets up Python virtual environment and installs dependencies

set -euo pipefail

check_python_environment() {

    # Check if python is available
    if ! command -v python3 &> /dev/null; then
        echo "❌ Error: python3 is not available"
        echo "Please install Python 3.10+ to continue"
        exit 1
    fi

    echo "✅ Python found: $(python3 --version)"

    # Check if pip is available
    if ! command -v pip3 &> /dev/null && ! python3 -m pip --version &> /dev/null; then
        echo "❌ Error: pip is not available"
        echo "Please install pip to continue"
        exit 1
    fi

    echo "✅ Pip found"

    # Check Python version is 3.10+
    PYTHON_VERSION=$(python3 -c "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')")
    REQUIRED_VERSION="3.10"

    if ! python3 -c "import sys; exit(0 if sys.version_info >= (3, 10) else 1)"; then
        echo "❌ Error: Python version $PYTHON_VERSION is less than required $REQUIRED_VERSION"
        echo "Please upgrade to Python 3.10+ to continue"
        exit 1
    fi

    echo "✅ Python version $PYTHON_VERSION meets requirements"

}

echo "🔧 Initializing E2E test environment..."

# Check if venv directory exists at base directory
VENV_PATH="./venv"
if [ ! -d "$VENV_PATH" ]; then
    # Create one.

    # 1. Check python environment
    check_python_environment
    
    # 2. Create venv
    python3 -m venv venv
    
    # 3. Activate venv
    source venv/bin/activate

    # 4. Install dependencies
    pip install -r tests/e2e/requirements.txt

fi

# Source into venv
echo "🔄 Activating virtual environment..."
source "$VENV_PATH/bin/activate"

# Verify we're in the virtual environment
if [ -z "${VIRTUAL_ENV:-}" ]; then
    echo "❌ Error: Failed to activate virtual environment"
    exit 1
fi

echo "✅ Virtual environment activated: $VIRTUAL_ENV"

# Install E2E test requirements
REQUIREMENTS_FILE="tests/e2e/requirements.txt"
if [ ! -f "$REQUIREMENTS_FILE" ]; then
    echo "❌ Error: Requirements file not found at $REQUIREMENTS_FILE"
    exit 1
fi

echo "📦 Installing E2E test dependencies..."
pip install -r "$REQUIREMENTS_FILE"

echo "🎉 E2E test environment initialized successfully!"
echo "💡 To activate the environment manually, run: source venv/bin/activate"