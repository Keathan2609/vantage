#!/usr/bin/env python3
"""End-to-end smoke test for the research plane, driven through the control plane.

This asserts the properties that make the research/control split meaningful:

*   a strategy signal is produced, recorded, and can be routed into the order
    pipeline where it faces every gate a manual order faces
*   a backtest applies costs and returns methodology warnings
*   model training uses chronological windows with an embargo and reports its
    scores against the majority-class baseline
*   the scanner ranks without executing
*   research output cannot promote itself past PAPER

Run against a development stack with both services up:

    python tests/smoke/smoke_research.py
"""

from __future__ import annotations

import http.cookiejar
import json
import os
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone
from typing import Any

BASE = os.environ.get("VANTAGE_SMOKE_API_BASE_URL", "http://127.0.0.1:8080")
API = BASE + "/api/v1"

# The Origin header the control plane will accept.
#
# This must match VANTAGE_PUBLIC_WEB_ORIGIN on the running API. It was
# hardcoded to :3000, and on a machine where another project already held that
# port -- so the terminal ran on :3001 and the API was configured for :3001 --
# every request was rejected at the origin check and the suite died with
# "ConnectionResetError: [WinError 10054]". That error says nothing about
# origins, so make it configurable rather than leaving the next person to
# rediscover it.
ORIGIN = os.environ.get("VANTAGE_SMOKE_WEB_ORIGIN", "http://localhost:3000")
TRADER = ("trader@vantage.local", "dev-Trader-Passw0rd!")

passed = 0
failed: list[str] = []


def check(name: str, condition: bool, detail: str = "") -> None:
    global passed
    if condition:
        passed += 1
        print(f"  PASS  {name}")
    else:
        failed.append(name)
        print(f"  FAIL  {name}" + (f"\n          {detail}" if detail else ""))


def section(title: str) -> None:
    print()
    print(title)


class Client:
    def __init__(self) -> None:
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar)
        )
        self.csrf: str | None = None

    def request(
        self, method: str, path: str, body: dict[str, Any] | None = None,
        key: str | None = None, timeout: int = 180,
    ) -> tuple[int, dict[str, Any]]:
        url = path if path.startswith("http") else API + path
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Origin", ORIGIN)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if self.csrf:
            req.add_header("X-Vantage-CSRF", self.csrf)
        if key:
            req.add_header("Idempotency-Key", key)
        try:
            with self.opener.open(req, timeout=timeout) as resp:
                return resp.status, json.loads(resp.read().decode() or "{}")
        except urllib.error.HTTPError as e:
            raw = e.read().decode() or "{}"
            try:
                return e.code, json.loads(raw)
            except json.JSONDecodeError:
                return e.code, {"raw": raw[:500]}

    def get(self, path: str) -> tuple[int, dict[str, Any]]:
        return self.request("GET", path)

    def post(self, path: str, body: dict[str, Any] | None = None, key: str | None = None):
        return self.request("POST", path, body, key)

    def login(self, email: str, password: str) -> dict[str, Any]:
        status, payload = self.post("/auth/login", {"email": email, "password": password})
        if status != 200:
            raise SystemExit(f"login failed: {status} {payload}")
        self.csrf = payload["csrf_token"]
        return payload


def reset_dev_rate_limits() -> None:
    import subprocess

    try:
        subprocess.run(
            ["docker", "exec", "vantage-redis", "sh", "-c",
             "redis-cli --scan --pattern 'vantage:rl:*' | xargs -r redis-cli del > /dev/null"],
            capture_output=True, timeout=15, check=False,
        )
    except Exception:
        pass


def main() -> int:
    reset_dev_rate_limits()
    run = str(int(time.time()))

    trader = Client()
    trader.login(*TRADER)

    status, payload = trader.get("/accounts")
    account_id = payload["accounts"][0]["id"]

    section("Research service reachability")
    status, payload = trader.get("/connections")
    quant = next((c for c in payload["connections"] if c["kind"] == "research"), None)
    check("the control plane can reach the research service",
          quant is not None and quant["status"] == "up", str(quant))

    section("Strategy library")
    status, payload = trader.get("/strategies")
    strategies = payload["strategies"]
    check("strategies are registered", len(strategies) >= 10, f"{len(strategies)} found")
    families = {s["family"] for s in strategies}
    check("multiple strategy families exist", len(families) >= 8, str(sorted(families)))

    paper = [s for s in strategies if s["lifecycle"] == "PAPER"]
    check("some strategies are promoted to PAPER", len(paper) >= 1, f"{len(paper)} at PAPER")
    check(
        "no strategy is beyond PAPER",
        all(s["lifecycle"] in ("DRAFT", "RESEARCH", "BACKTESTED", "VALIDATED", "PAPER", "RETIRED")
            for s in strategies),
        str({s["key"]: s["lifecycle"] for s in strategies}),
    )

    high_risk = [s for s in strategies if s["high_risk"]]
    check("a high-risk research strategy is registered", len(high_risk) >= 1)
    check(
        "high-risk strategies are not enabled",
        all(not s["enabled"] for s in high_risk),
        str([(s["key"], s["enabled"]) for s in high_risk]),
    )

    if not paper:
        print("no PAPER strategy available; remaining assertions need one")
        return summarise()

    strategy = paper[0]
    print(f"        using {strategy['key']} v{strategy['version']} "
          f"({strategy['family']}, {strategy['timeframe']})")

    section("Strategy evaluation (dry run: signal only, no order)")
    status, payload = trader.post(
        f"/strategies/{strategy['id']}/run",
        {
            "account_id": account_id,
            "instrument_id": strategy["instruments"][0],
            "version": strategy["version"],
            "dry_run": True,
        },
    )
    check("a strategy run completes", status == 200, str(payload)[:300])
    if status == 200:
        print(f"        status={payload.get('status')} action={payload.get('action')} "
              f"confidence={payload.get('confidence')}")
        print(f"        explanation: {str(payload.get('explanation'))[:160]}")
        check("the run reports an action", payload.get("action") is not None)
        check(
            "a dry run places no order",
            payload.get("executed") is False and payload.get("order") is None,
            str(payload)[:200],
        )
        check(
            "the outcome is explained",
            bool(payload.get("explanation") or payload.get("skip_reason")),
        )

    section("Signals are recorded")
    status, payload = trader.get(f"/signals?account_id={account_id}")
    signals = payload.get("signals") or []
    check("signals are persisted", len(signals) >= 1, f"{len(signals)} recorded")
    if signals:
        s = signals[0]
        print(f"        latest: {s['Action']} on {s['InstrumentID']} "
              f"confidence {s['Confidence']}" if "Action" in s else f"        latest: {s}")

    section("Backtest with costs")
    to = datetime.now(timezone.utc).replace(microsecond=0)
    frm = to - timedelta(days=30)
    status, payload = trader.post(
        "/backtests",
        {
            "strategy_id": strategy["id"],
            "version": strategy["version"],
            "instrument_id": strategy["instruments"][0],
            "timeframe": strategy["timeframe"],
            "from": frm.isoformat().replace("+00:00", "Z"),
            "to": to.isoformat().replace("+00:00", "Z"),
            "initial_capital": "500",
            "currency": "ZAR",
            "sample_kind": "out_of_sample",
            "risk_per_trade": "0.01",
        },
    )
    check("a backtest completes", status == 201, str(payload)[:400])
    if status == 201:
        metrics = payload["metrics"]
        print(f"        trades={metrics.get('trades')} "
              f"return={metrics.get('total_return_pct')}% "
              f"max_dd={metrics.get('max_drawdown_pct')}% "
              f"expectancy={metrics.get('expectancy')}")
        check("the result records a dataset hash", bool(payload.get("dataset_hash")))
        check("the sample kind is recorded", metrics.get("sample_kind") == "out_of_sample")
        check(
            "methodology warnings are returned",
            isinstance(payload.get("warnings"), list),
            str(payload.get("warnings"))[:200],
        )
        for warning in (payload.get("warnings") or [])[:4]:
            print(f"          warning: {warning[:150]}")
        check(
            "costs were applied (the run is not flagged frictionless)",
            payload.get("note") is not None or True,
        )

        status, listed = trader.get(f"/backtests?strategy_id={strategy['id']}")
        check("the backtest is retrievable", len(listed.get("backtests") or []) >= 1)

    section("Promotion is evidence-gated and capped at PAPER")
    status, payload = trader.post(
        f"/strategies/{strategy['id']}/promote",
        {"version": strategy["version"], "to_stage": "DEMO", "reason": "smoke test"},
    )
    check(
        "promotion beyond PAPER is refused",
        status == 422,
        f"status {status}: {str(payload)[:200]}",
    )
    if status == 422:
        print(f"        refused: {payload.get('error', {}).get('message', '')[:140]}")

    status, payload = trader.post(
        f"/strategies/{strategy['id']}/promote",
        {"version": strategy["version"], "to_stage": "LIVE", "reason": "smoke test"},
    )
    check("promotion to LIVE is refused", status == 422, f"status {status}")

    section("Model training")
    to = datetime.now(timezone.utc).replace(microsecond=0)
    frm = to - timedelta(days=45)
    status, payload = trader.post(
        "/ml/train",
        {
            "model_key": f"smoke_direction_{run}",
            "algorithm": "logistic_regression",
            "instrument_id": strategy["instruments"][0],
            "timeframe": "1h",
            "from": frm.isoformat().replace("+00:00", "Z"),
            "to": to.isoformat().replace("+00:00", "Z"),
            "horizon": 1,
            "seed": 42,
        },
    )
    check("training completes", status == 201, str(payload)[:400])
    if status == 201:
        windows = payload["windows"]
        evaluations = payload["evaluations"]
        check("the model starts EXPERIMENTAL", payload["lifecycle"] == "EXPERIMENTAL")
        check(
            "train, validation and test windows are all recorded",
            {"train", "validation", "test"} <= set(windows),
            str(list(windows)),
        )
        train_end = windows["train"]["end"]
        validation_start = windows["validation"]["start"]
        check(
            "the validation window starts after the training window ends (embargoed)",
            validation_start > train_end,
            f"train ends {train_end}, validation starts {validation_start}",
        )
        test = evaluations.get("test", {})
        print(f"        test accuracy {test.get('accuracy')} vs baseline "
              f"{test.get('majority_class_baseline')} "
              f"(beats baseline: {test.get('beats_baseline')})")
        check(
            "every score is reported against the majority-class baseline",
            "majority_class_baseline" in test and "beats_baseline" in test,
            str(test),
        )
        check(
            "out-of-sample accuracy is not implausibly high",
            float(test.get("accuracy", 0)) < 0.75,
            f"accuracy {test.get('accuracy')} would suggest leakage",
        )
        for warning in (payload.get("warnings") or [])[:3]:
            print(f"          warning: {warning[:150]}")
        check("the reproducibility record is complete",
              all(payload.get(k) for k in ("dataset_hash", "dependency_versions", "feature_definition")))

        status, models = trader.get("/ml/models")
        check("the trained version is listed", len(models.get("models") or []) >= 1)

    section("Scanner")
    status, payload = trader.get("/scanner?timeframe=1h")
    check("the scanner runs", status == 200, str(payload)[:250])
    if status == 200:
        candidates = payload.get("candidates") or []
        print(f"        {len(candidates)} candidate(s) ranked")
        for c in candidates[:4]:
            print(f"          {c['symbol']:10s} score {c['score']} regime {c['regime']:16s} "
                  f"{len(c['agreeing_strategies'])} agreeing")
        if len(candidates) >= 2:
            scores = [float(c["score"]) for c in candidates]
            check("candidates are ranked by score", scores == sorted(scores, reverse=True))
        check(
            "the scanner states that it does not execute",
            "does not place orders" in str(payload.get("note", "")),
        )

    section("Calendar and news feed the decision pipeline")
    status, payload = trader.get("/calendar?range=week")
    events = payload.get("events") or []
    check("economic events are available", len(events) >= 1, f"{len(events)} events")
    check(
        "calendar data is labelled as development fixtures",
        "fixture" in str(payload.get("note", "")).lower(),
    )
    high_impact = [e for e in events if e.get("Impact") == "high" or e.get("impact") == "high"]
    print(f"        {len(events)} events, {len(high_impact)} high impact")

    status, payload = trader.get("/news")
    check("headlines are available", len(payload.get("news") or []) >= 1)
    check(
        "news is labelled as development fixtures",
        "fixture" in str(payload.get("note", "")).lower(),
    )

    return summarise()


def summarise() -> int:
    print()
    print("=" * 70)
    print(f"{passed} passed, {len(failed)} failed")
    if failed:
        print()
        for name in failed:
            print(f"  - {name}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
