#!/bin/bash

script_dir=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )

source "${script_dir}/../venv/bin/activate"

python3 -c "import sys; sys.path.append('tests/e2e'); from utils import wait_for_cluster_convergence; import time; print('🔄 Testing cluster convergence...'); start = time.time(); result = wait_for_cluster_convergence(timeout=120); elapsed = time.time() - start; print(f'✅ Cluster converged in {elapsed:.1f}s: {result}')"
