"""
pytest configuration for Memgraph E2E tests
"""

import pytest
from utils import log_info, E2ETestError


def pytest_configure(config):
    """Configure pytest for e2e tests"""
    # Add custom markers
    config.addinivalue_line("markers", "basic: basic functionality tests")
    config.addinivalue_line("markers", "failover: failover scenario tests")
    config.addinivalue_line("markers", "restart: rolling restart tests")
    config.addinivalue_line("markers", "slow: tests that take longer to run")


def pytest_sessionstart(session):
    """Start test session"""
    log_info("🚀 Starting Memgraph E2E Tests (Python)")
    log_info("=" * 50)


def pytest_runtest_setup(item):
    """Setup for each test"""
    log_info(f"🧪 Starting test: {item.name}")


def pytest_runtest_teardown(item, nextitem):
    """Teardown for each test"""
    # Add any cleanup logic here if needed
    pass


@pytest.fixture(scope="session")
def cluster_ready():
    """Fixture to ensure cluster is ready for testing"""
    from utils import wait_for_cluster_ready

    try:
        wait_for_cluster_ready()
        return True
    except E2ETestError as e:
        pytest.skip(f"Cluster not ready: {e}")


@pytest.fixture
def unique_test_id():
    """Generate unique test ID for each test"""
    from utils import generate_test_id
    return generate_test_id()
