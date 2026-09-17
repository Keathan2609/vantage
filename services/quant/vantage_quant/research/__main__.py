"""Run a research experiment from committed code.

The previous milestone's experiment was driven by a throwaway script, which
means the artefact and the code that produced it were never in the repository
together. A result nobody can re-run is not a result, so the entry point lives
here and the run parameters are arguments rather than edits.

    python -m vantage_quant.research expanded --bars 1200 --out run.json

Nothing here reaches a network, a broker or a database. It generates bars,
evaluates the registered strategies over them, and writes JSON.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="vantage_quant.research")
    sub = parser.add_subparsers(dest="command", required=True)

    expanded_cmd = sub.add_parser(
        "expanded", help="the expanded synthetic experiment (signal coverage, power)"
    )
    expanded_cmd.add_argument(
        "--bars", type=int, default=1200,
        help="bars per market condition; 16 conditions are generated",
    )
    expanded_cmd.add_argument(
        "--workers", type=int, default=None,
        help="worker processes; defaults to min(12, cpu_count)",
    )
    expanded_cmd.add_argument(
        "--out", type=Path, default=None, help="write JSON here instead of stdout"
    )

    args = parser.parse_args(argv)

    if args.command == "expanded":
        from . import expanded

        result = expanded.run(bars_per_condition=args.bars, workers=args.workers)
        payload = expanded.to_json(result)
        if args.out:
            args.out.write_text(payload, encoding="utf-8")
            # To stderr, so stdout stays a clean JSON stream when it is used.
            print(f"wrote {args.out} ({len(payload)} bytes)", file=sys.stderr)
        else:
            print(payload)
        return 0

    return 2


# ProcessPoolExecutor uses spawn on Windows, which re-imports __main__ in every
# worker. Without this guard each worker would start its own pool.
if __name__ == "__main__":
    raise SystemExit(main())
