"""Run a research experiment from committed code.

An earlier experiment was driven by a throwaway script, which means the
artefact and the code that produced it were never in the repository together.
A result nobody can re-run is not a result, so the entry point lives here and
the run parameters are arguments rather than edits.

    python -m vantage_quant.research expanded --bars 1200 --out run.json
    python -m vantage_quant.research historical --instrument XAUUSD --out hist.json
    python -m vantage_quant.research datasets

Nothing here reaches a network, a broker or a database. It reads local files,
evaluates the registered strategies over them, and writes JSON.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path


def _expanded(args: argparse.Namespace) -> int:
    from . import expanded

    result = expanded.run(bars_per_condition=args.bars, workers=args.workers)
    payload = expanded.to_json(result)
    _emit(payload, args.out)
    return 0


def _historical(args: argparse.Namespace) -> int:
    from . import historical_run, requirements

    try:
        result = historical_run.run(
            instrument=args.instrument,
            timeframe=args.timeframe,
            provider=args.provider,
            acquisition_method=args.acquisition_method,
            licensing_note=args.licensing_note,
            declared_timezone=args.declared_timezone,
            workers=args.workers,
        )
    except historical_run.NoHistoricalDataError as exc:
        # Deliberately not an error exit: "no data yet" is a legitimate state
        # of this milestone, and the specification of what is needed is the
        # useful output. Nothing is fetched to paper over it.
        print(exc, file=sys.stderr)
        print(
            requirements.waiting_report(
                instrument=args.instrument, timeframe=args.timeframe
            )
        )
        return 0

    _emit(historical_run.to_json(result), args.out)
    return 0


def _datasets(args: argparse.Namespace) -> int:
    """What is present, its quality, and what is still needed.

    The minimal research view called for by the brief: a text table rather
    than a screen, because the question "is this data fit to conclude from"
    is answered by the numbers, not by the presentation.
    """
    from . import historical, historical_run, quality, requirements

    found = historical.available()
    print(historical.status())
    if not found:
        print()
        print(requirements.waiting_report())
        return 0

    imported = historical_run.import_all(
        instrument=args.instrument, timeframe=args.timeframe
    )
    assignments = {a.dataset_id: a for a in historical_run._assign(imported)}

    print()
    header = (
        f"{'dataset':22} {'status':20} {'partition':11} {'bars':>7} "
        f"{'coverage':>9} {'missing':>8} {'range'}"
    )
    print(header)
    print("-" * len(header))
    for dataset in imported:
        q = dataset.quality_report
        a = assignments.get(dataset.dataset_id)
        coverage = (
            f"{q.coverage_fraction:.1%}" if q.coverage_fraction is not None else "-"
        )
        print(
            f"{dataset.dataset_id:22} {dataset.status.value:20} "
            f"{(a.partition.value if a else 'EXCLUDED'):11} {q.rows:7} "
            f"{coverage:>9} {q.missing_bars if q.missing_bars is not None else '-'!s:>8} "
            f"{q.first_bar[:16]} .. {q.last_bar[:16]}"
        )
        print(f"{'':22} sha256(raw)={dataset.provenance.original_file_hash[:16]}… "
              f"provider={dataset.provenance.provider} "
              f"costs={dataset.provenance.cost_basis}")
        for finding in q.errors:
            print(f"{'':22}   ERROR   {finding.code} x{finding.count}")
        for finding in q.warnings:
            print(f"{'':22}   WARNING {finding.code} x{finding.count}")
        if a and a.seal:
            print(f"{'':22}   {a.seal} -- not read by this milestone")

    rejected = [
        d for d in imported if d.status is quality.DatasetStatus.REJECTED
    ]
    if rejected:
        print()
        print(
            f"{len(rejected)} dataset(s) REJECTED and excluded from research. "
            f"Nothing was repaired."
        )
    return 0


def _compare(args: argparse.Namespace) -> int:
    """Line up a synthetic run and a historical run, strategy by strategy.

    Reads two run artefacts rather than recomputing either, so the table
    describes exactly the runs that were recorded.
    """
    import json

    from . import compare as compare_mod
    from . import verdict

    def sides(path: Path, source_type: str) -> tuple[dict, verdict.AnalysisVersions]:
        payload = json.loads(path.read_text(encoding="utf-8"))
        versions = verdict.AnalysisVersions(**payload["analysis_versions"])
        built = {}
        for row in payload["reports"]:
            built[row["strategy_id"]] = compare_mod.Side(
                source_type=source_type,
                verdict=row["readiness"],
                effective_n=row["effective_n"],
                raw_n=row["raw_n"],
                spearman=_strongest(row, "spearman"),
                spearman_excludes_zero=_excludes_zero(row, "spearman_ci"),
                score_span_used=row["score_span_used"],
                mean_net_return=_strongest(row, "mean_net_return") or 0.0,
                net_survives_costs=_excludes_zero(row, "net_return_ci", positive=True),
                regimes=tuple(sorted(row.get("by_regime", {}))),
            )
        return built, versions

    synthetic, synthetic_versions = sides(args.synthetic, "SYNTHETIC_CONTROLLED")
    historical, historical_versions = sides(args.historical, "HISTORICAL_MARKET")

    rows = compare_mod.compare(
        synthetic=synthetic,
        historical=historical,
        synthetic_versions=synthetic_versions,
        historical_versions=historical_versions,
    )
    for row in rows:
        left, right = row.synthetic, row.historical
        print(
            f"{row.strategy_id:28} "
            f"syn={left.verdict if left else '-':34} "
            f"hist={right.verdict if right else '-':34} "
            f"{row.disagreement.value}"
        )
        print(f"{'':28} {row.detail}")
    return 0


def _strongest(row: dict, field: str) -> float | None:
    """The horizon the verdict was drawn from, not the most flattering one."""
    best = None
    for horizon in row.get("horizons", []):
        value = horizon.get("spearman")
        if value is None:
            continue
        if best is None or abs(value) > abs(best.get("spearman") or 0.0):
            best = horizon
    return None if best is None else best.get(field)


def _excludes_zero(row: dict, field: str, *, positive: bool = False) -> bool:
    best = None
    for horizon in row.get("horizons", []):
        value = horizon.get("spearman")
        if value is None:
            continue
        if best is None or abs(value) > abs(best.get("spearman") or 0.0):
            best = horizon
    interval = (best or {}).get(field)
    if not interval:
        return False
    lower, upper = float(interval["lower"]), float(interval["upper"])
    excludes = (lower > 0 and upper > 0) or (lower < 0 and upper < 0)
    if positive:
        return excludes and float(interval["point"]) > 0
    return excludes


def _emit(payload: str, out: Path | None) -> None:
    if out:
        out.write_text(payload, encoding="utf-8")
        # To stderr, so stdout stays a clean JSON stream when it is used.
        print(f"wrote {out} ({len(payload)} bytes)", file=sys.stderr)
    else:
        print(payload)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="vantage_quant.research")
    sub = parser.add_subparsers(dest="command", required=True)

    expanded_cmd = sub.add_parser(
        "expanded", help="the expanded SYNTHETIC experiment (coverage, power)"
    )
    expanded_cmd.add_argument(
        "--bars", type=int, default=1200,
        help="bars per market condition; 16 conditions are generated",
    )
    expanded_cmd.add_argument(
        "--workers", type=int, default=None,
        help="worker processes; defaults to min(12, cpu_count)",
    )
    expanded_cmd.add_argument("--out", type=Path, default=None)
    expanded_cmd.set_defaults(handler=_expanded)

    hist = sub.add_parser(
        "historical", help="the HISTORICAL experiment over local market data"
    )
    hist.add_argument("--instrument", default="XAUUSD")
    hist.add_argument("--timeframe", default="1h")
    hist.add_argument(
        "--provider", default="UNRECORDED",
        help="who supplied the data. Recorded verbatim; never guessed",
    )
    hist.add_argument("--acquisition-method", default="UNRECORDED")
    hist.add_argument("--licensing-note", default="UNRECORDED")
    hist.add_argument(
        "--declared-timezone", default=None,
        help="the timezone the SOURCE states its timestamps are in. Omitted "
             "means nobody said, which is recorded as an assumption rather "
             "than resolved from this machine's locale",
    )
    hist.add_argument(
        "--workers", type=int, default=None,
        help="worker processes; defaults to min(12, cpu_count)",
    )
    hist.add_argument("--out", type=Path, default=None)
    hist.set_defaults(handler=_historical)

    listing = sub.add_parser(
        "datasets", help="what historical data is present, and what is needed"
    )
    listing.add_argument("--instrument", default="XAUUSD")
    listing.add_argument("--timeframe", default="1h")
    listing.set_defaults(handler=_datasets)

    comparison = sub.add_parser(
        "compare",
        help="synthetic versus historical, side by side and never averaged",
    )
    comparison.add_argument("--synthetic", type=Path, required=True)
    comparison.add_argument("--historical", type=Path, required=True)
    comparison.set_defaults(handler=_compare)

    args = parser.parse_args(argv)
    handler = args.handler
    return int(handler(args))


# ProcessPoolExecutor uses spawn on Windows, which re-imports __main__ in every
# worker. Without this guard each worker would start its own pool.
if __name__ == "__main__":
    raise SystemExit(main())
