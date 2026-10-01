# Research plane image.
#
# The research service has no broker client and no credentials pointing back at
# the control plane. Its database role can read market and research tables and
# nothing else. The container reflects that: non-root, read-only root
# filesystem, no build toolchain in the final stage.
#
# Build from services/quant:
#   docker build -t vantage/quant .

# ---------------------------------------------------------------- build stage
FROM python:3.12.12-slim-trixie AS build

ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1

# Build tooling for any wheel that needs compiling. It stays in this stage.
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# A virtualenv rather than a system install, so the final stage copies one
# self-contained directory.
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

COPY pyproject.toml ./
COPY vantage_quant ./vantage_quant

RUN pip install --upgrade pip setuptools wheel \
    && pip install .

# ---------------------------------------------------------------- final stage
FROM python:3.12.12-slim-trixie

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PATH="/opt/venv/bin:$PATH"

# Apply the distribution's security updates. A base image tag is a snapshot,
# and openssl or util-linux fixes published after that snapshot are not in it.
# This trades strict byte-for-byte reproducibility for a patched image, which
# is the right way round for something reachable over a network.
RUN apt-get update \
    && apt-get upgrade -y --no-install-recommends \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# The base image's own pip and setuptools are older than the application's.
# They are not used at runtime -- the service runs out of the copied virtualenv
# -- but they are still installed files a scanner will flag, and `pkg_resources`
# is imported by enough libraries that deleting setuptools is riskier than
# updating it.
RUN pip install --no-cache-dir --upgrade pip setuptools

# A dedicated unprivileged account, and a writable artifact directory that is
# the only path this service needs to write to.
RUN groupadd --system --gid 10001 vantage \
    && useradd --system --uid 10001 --gid vantage --no-create-home vantage \
    && mkdir -p /var/lib/vantage/artifacts \
    && chown -R vantage:vantage /var/lib/vantage

COPY --from=build /opt/venv /opt/venv

WORKDIR /app
COPY --chown=vantage:vantage vantage_quant ./vantage_quant

USER vantage:vantage

EXPOSE 8000

# urllib rather than curl: the image has no curl, and adding one to satisfy a
# healthcheck would put a network client in the container for no other reason.
HEALTHCHECK --interval=10s --timeout=5s --start-period=20s --retries=6 \
    CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:8000/health/live', timeout=3).status==200 else 1)"

# One worker. The service is CPU-bound on numeric work and is called by a
# single control plane; more workers would multiply memory for no throughput.
CMD ["python", "-m", "uvicorn", "vantage_quant.main:app", \
     "--host", "0.0.0.0", "--port", "8000", "--workers", "1", "--no-server-header"]
