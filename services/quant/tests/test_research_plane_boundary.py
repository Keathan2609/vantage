"""The research plane cannot reach a database, a broker or the control plane.

This is rule 5 of the engineering guide, written as a test rather than left as
an intention: *Python must never reach a broker. No adapter, no credential
pointing at the control plane, no write access to trading tables.*

The Go side has two arch tests for the equivalent boundary
(`TestOnlyBookingAppendsFills`, `TestBookingHoldsNoBrokerAdapter`). The Python
side had none, and the gap was not theoretical. Both compose files handed this
service `VANTAGE_QUANT_READONLY_DATABASE_URL`, and the deployment guide walked
the operator through constructing it -- for a service that has never had a
database driver installed and so could not have used it. Nothing failed,
because nothing read it. The danger was the next person to add `psycopg` for a
good reason, who would inherit a live connection string already sitting in the
container's environment, and a connection that happens without anyone deciding
it should.

These tests fail the moment that becomes possible again. A failure is not
necessarily a bug -- it is a design decision that now has to be made
deliberately and in the open, which is the whole point.
"""

from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest

PACKAGE_ROOT = Path(__file__).resolve().parent.parent / "vantage_quant"

# Drivers, not libraries that merely talk to a network. The research service is
# an HTTP service and obviously has an HTTP stack; what it must not have is a
# way to open a connection to the trading database.
DATABASE_DRIVERS = (
    "psycopg",
    "psycopg2",
    "asyncpg",
    "pg8000",
    "sqlalchemy",
    "sqlmodel",
    "pymysql",
    "aiomysql",
)


@pytest.mark.parametrize("driver", DATABASE_DRIVERS)
def test_no_database_driver_is_installed(driver: str) -> None:
    """Not importable, so a connection cannot be opened by accident.

    `find_spec` rather than a try/import: it answers "is this installable and
    present" without executing the module, and it does not care whether some
    other test already imported it.
    """
    assert importlib.util.find_spec(driver) is None, (
        f"{driver} is installed in the research plane. Rule 5 says Python never "
        f"reaches the trading database. If this is deliberate, the boundary has "
        f"moved and the engineering guide has to move with it."
    )


def test_no_module_references_a_database_driver() -> None:
    """Installed is one question; imported is another.

    A driver could arrive transitively tomorrow without anyone adding it on
    purpose, and the test above would then fail for a reason that has nothing to
    do with this package's own code. This one looks at what the package itself
    asks for, which is the thing the authors control.
    """
    offenders: list[str] = []
    for source in sorted(PACKAGE_ROOT.rglob("*.py")):
        text = source.read_text(encoding="utf-8")
        for driver in DATABASE_DRIVERS:
            if f"import {driver}" in text or f"from {driver}" in text:
                offenders.append(f"{source.relative_to(PACKAGE_ROOT)}: {driver}")

    assert not offenders, "research modules import a database driver: " + ", ".join(
        offenders
    )


def test_no_module_reads_a_database_connection_string() -> None:
    """A credential read is the step before a connection.

    Catching the environment read as well as the driver import means a
    half-finished change -- the configuration wired up, the client not yet
    added -- is caught at the point where the credential enters the process,
    rather than later when something finally uses it.
    """
    offenders: list[str] = []
    for source in sorted(PACKAGE_ROOT.rglob("*.py")):
        text = source.read_text(encoding="utf-8")
        for marker in ("DATABASE_URL", "POSTGRES_PASSWORD", "PGPASSWORD"):
            if marker in text:
                offenders.append(f"{source.relative_to(PACKAGE_ROOT)}: {marker}")

    assert not offenders, "research modules read a database credential: " + ", ".join(
        offenders
    )
