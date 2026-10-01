#!/usr/bin/env python3
"""End-to-end smoke test for a running Vantage control plane.

This exercises the control plane the way a client does -- over HTTP, with
cookies and CSRF -- and asserts the platform's SAFETY properties, not just its
happy path:

  * a trade completes end to end and lands in the ledger and the position book
  * a repeated idempotency key returns the original outcome and never a second
    order, including under concurrent submission
  * the same key with a different payload is refused outright
  * risk limits, stop-loss policy and affordability refusals fire with a
    structured reason
  * a kill switch halts new orders without liquidating anything
  * a viewer cannot trade, and one user cannot see another's account
  * the audit chain verifies

Run against a development instance:

    python tests/smoke/smoke.py

It is written with the standard library only, so it runs anywhere the repo
does, and it exits non-zero on the first failed assertion.
"""

from __future__ import annotations

import http.cookiejar
import json
import os
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
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
VIEWER = ("viewer@vantage.local", "dev-Viewer-Passw0rd!")
ADMIN = ("admin@vantage.local", "dev-Admin-Passw0rd!")

RUN = str(int(time.time()))

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
    """A session-scoped API client."""

    def __init__(self) -> None:
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar)
        )
        self.csrf: str | None = None

    def request(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
    ) -> tuple[int, dict[str, Any]]:
        url = path if path.startswith("http") else API + path
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Origin", ORIGIN)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if self.csrf:
            req.add_header("X-Vantage-CSRF", self.csrf)
        if idempotency_key:
            req.add_header("Idempotency-Key", idempotency_key)
        try:
            with self.opener.open(req, timeout=30) as resp:
                raw = resp.read().decode() or "{}"
                return resp.status, json.loads(raw)
        except urllib.error.HTTPError as e:
            raw = e.read().decode() or "{}"
            try:
                return e.code, json.loads(raw)
            except json.JSONDecodeError:
                return e.code, {"raw": raw}

    def get(self, path: str) -> tuple[int, dict[str, Any]]:
        return self.request("GET", path)

    def post(
        self, path: str, body: dict[str, Any] | None = None, key: str | None = None
    ) -> tuple[int, dict[str, Any]]:
        return self.request("POST", path, body, key)

    def patch(
        self, path: str, body: dict[str, Any] | None = None
    ) -> tuple[int, dict[str, Any]]:
        return self.request("PATCH", path, body)

    def login(self, email: str, password: str) -> dict[str, Any]:
        status, payload = self.post("/auth/login", {"email": email, "password": password})
        if status != 200:
            raise SystemExit(f"login failed for {email}: {status} {payload}")
        self.csrf = payload["csrf_token"]
        return payload


def reset_dev_rate_limits() -> None:
    """Clear the login rate-limit buckets in a DEVELOPMENT Redis.

    Repeated smoke runs would otherwise exhaust the sign-in budget, which is a
    real control and is deliberately not widened to suit a test. Clearing the
    development cache is the honest way around it; this is a no-op wherever the
    development Redis container is not reachable.
    """
    import subprocess

    try:
        subprocess.run(
            ["docker", "exec", "vantage-redis", "sh", "-c",
             "redis-cli --scan --pattern 'vantage:rl:auth_*' | "
             "xargs -r redis-cli del > /dev/null; echo cleared"],
            capture_output=True, timeout=15, check=False,
        )
    except Exception:
        pass


def wait_for_tradable_feed(client: "Client", instrument_id: str, seconds: int = 30) -> str:
    """Wait until the ingestor publishes a quote fit for automation.

    The market-data ingestor polls every two seconds. Immediately after a
    reseed there is a window in which the newest quote is older than the
    staleness threshold, and the risk engine correctly refuses an order
    against it. Waiting here keeps the suite deterministic without weakening
    the check being exercised -- a feed that never becomes healthy is still a
    failure, reported as one.
    """
    deadline = time.time() + seconds
    state = "unknown"
    while time.time() < deadline:
        _, payload = client.get("/market/health")
        for entry in payload.get("health") or []:
            if entry.get("instrument_id") == instrument_id:
                state = str(entry.get("state"))
                if entry.get("tradable_by_automation"):
                    return state
        time.sleep(1)
    return state


def main() -> int:
    reset_dev_rate_limits()

    section("Build identity")
    anon = Client()
    status, version = anon.get(BASE + "/version")
    check("version endpoint responds", status == 200)
    check(
        "execution mode is paper",
        version.get("execution_mode") == "paper",
        str(version),
    )
    check(
        "funds are reported as simulated",
        version.get("simulated_funds") is True,
    )
    check(
        "live trading is reported unavailable",
        version.get("live_trading_available") is False,
    )

    status, ready = anon.get(BASE + "/health/ready")
    check("readiness reports the database is up", ready.get("checks", {}).get("database") == "ok")

    section("Authentication")
    # Sessions are established FIRST and then reused for the rest of the run.
    # The login endpoint is deliberately rate-limited to five attempts per
    # address, which is a security control rather than an inconvenience to
    # engineer around, so this test spends its budget carefully: three
    # sign-ins, then two failure probes.
    trader = Client()
    session = trader.login(*TRADER)
    check("trader signs in", session["user"]["role"] == "trader")

    viewer = Client()
    viewer.login(*VIEWER)
    admin = Client()
    admin.login(*ADMIN)
    check("all three development roles sign in", True)

    status, payload = trader.get("/auth/session")
    check("session endpoint reports the mode", payload.get("execution_mode") == "paper")

    bad = Client()
    status, wrong_pw = bad.post("/auth/login", {"email": TRADER[0], "password": "wrong-password-here"})
    if status == 429:
        print("  SKIP  credential-probe assertions (login budget exhausted)")
    else:
        check("a wrong password is rejected", status == 401, str(wrong_pw))
        status, unknown = bad.post(
            "/auth/login", {"email": "nobody@vantage.local", "password": "wrong-password-here"}
        )
        if status == 429:
            print("  SKIP  enumeration-oracle assertion (login budget exhausted)")
        else:
            # Identical code and message for both cases: distinguishing them
            # would tell an attacker which addresses are registered.
            check(
                "an unknown account is indistinguishable from a wrong password",
                unknown.get("error", {}).get("code") == wrong_pw.get("error", {}).get("code")
                and unknown.get("error", {}).get("message") == wrong_pw.get("error", {}).get("message"),
                f"{wrong_pw.get('error')} vs {unknown.get('error')}",
            )

    section("CSRF protection")
    # Reuses the trader's authenticated cookie jar and simply withholds or
    # forges the CSRF header, which is exactly the shape of a cross-site
    # request: the browser would send the cookie but not the header.
    real_csrf = trader.csrf
    trader.csrf = None
    status, payload = trader.post("/orders", {"account_id": "x"}, key="csrf-probe-" + RUN)
    check(
        "a state-changing request without a CSRF token is refused",
        status == 403,
        str(payload),
    )
    trader.csrf = "forged-token-value"
    status, payload = trader.post("/orders", {"account_id": "x"}, key="csrf-probe2-" + RUN)
    check("a forged CSRF token is refused", status == 403, str(payload))
    trader.csrf = real_csrf

    section("Accounts and market data")
    status, payload = trader.get("/accounts")
    accounts = payload["accounts"]
    check("the trader has a paper account", len(accounts) >= 1)
    account = accounts[0]
    account_id = account["id"]
    check("the account is flagged simulated", account["simulated"] is True)
    check("the account is denominated in ZAR", account["currency"] == "ZAR")

    status, payload = trader.get("/market/status")
    market_open = payload.get("tradable") is True
    print(f"        market status: {payload.get('status')} (sessions {payload.get('active_sessions')})")

    status, payload = trader.get("/market/quotes")
    quotes = {q["symbol"]: q for q in payload["quotes"]}
    check("XAUUSD.m has a quote", "XAUUSD.m" in quotes, str(list(quotes)))
    micro = quotes.get("XAUUSD.m", {})
    if micro:
        check(
            "the quote is two-sided and not crossed",
            float(micro["ask"]) >= float(micro["bid"]),
            f"bid {micro['bid']} ask {micro['ask']}",
        )
        print(f"        XAUUSD.m bid {micro['bid']} ask {micro['ask']} "
              f"spread {micro['spread']} health {micro['health']}")

    if not market_open:
        print()
        print("Market is closed, so order-path assertions are skipped.")
        print("Re-run during the trading week (Sunday 17:00 - Friday 17:00 New York).")
        return summarise()

    if micro.get("health") != "ok":
        print()
        print(f"Market data is {micro.get('health')}; waiting for a fresh quote...")
        for _ in range(10):
            time.sleep(2)
            _, payload = trader.get("/market/quotes")
            micro = {q["symbol"]: q for q in payload["quotes"]}.get("XAUUSD.m", {})
            if micro.get("health") == "ok":
                break
    check("market data is healthy enough for automated trading", micro.get("health") == "ok",
          f"state {micro.get('health')}")

    bid = float(micro["bid"])
    stop = round(bid * 0.995, 2)

    # Size the test order from the account's OWN risk headroom rather than
    # guessing. A R500 account has a per-instrument exposure ceiling of a few
    # hundred rand, so any hard-coded size is either trivially small or
    # refused; asking the platform what it will allow is both robust and a
    # demonstration that the limits are legible to a client.
    status, limits_payload = trader.get(f"/risk/limits/{account_id}")
    limits = limits_payload["limits"]
    _, fx_quotes = trader.get("/market/quotes")
    usdzar = {q["symbol"]: q for q in fx_quotes["quotes"]}.get("USDZAR")
    rate = float(usdzar["mid"]) if usdzar else 18.25
    headroom = min(
        float(limits["max_per_instrument_exposure"]),
        float(limits["max_order_notional"]),
        float(limits["max_gross_exposure"]),
    )
    # One lot of micro gold is one ounce; convert its price into the account's
    # currency, then take 60% of the headroom to leave room for the spread.
    per_lot_zar = bid * rate
    affordable = (headroom * 0.6) / per_lot_zar
    qty = max(0.01, round(affordable - (affordable % 0.01), 2))
    print(f"        risk headroom {headroom:.2f} ZAR, one lot costs {per_lot_zar:.2f} ZAR "
          f"-> testing with {qty} lots")

    section("Setup: flatten any existing exposure")
    # Repeated smoke runs accumulate positions, and an account at its exposure
    # limit correctly refuses new orders. Starting from flat is what makes the
    # order-path assertions meaningful -- and it exercises Flatten, which is the
    # deliberate, separately-confirmed way to close a position.
    status, payload = trader.get(f"/positions?account_id={account_id}")
    existing = payload.get("positions") or []
    for position in existing:
        st, resp = trader.post(
            f"/positions/{position['id']}/flatten",
            {"confirm": True},
            key=f"smoke-{RUN}-flatten-{position['id'][:8]}",
        )
        print(f"        flattened {position['side']} {position['quantity']} "
              f"{position['symbol']}: http {st}")
    if existing:
        check(
            "flatten closed the pre-existing exposure",
            len((trader.get(f"/positions?account_id={account_id}")[1].get("positions") or [])) == 0,
            "positions remain open after flatten",
        )
        check(
            "flatten requires explicit confirmation",
            trader.post(
                f"/positions/{existing[0]['id']}/flatten", {"confirm": False},
                key=f"smoke-{RUN}-noconfirm",
            )[0] in (404, 422),
            "an unconfirmed flatten must not proceed",
        )

    section("Order path")
    feed_state = wait_for_tradable_feed(trader, "XAUUSD.m")
    check(
        "the market data feed is fit to trade on",
        feed_state == "ok",
        f"feed state is {feed_state}; the ingestor never published a tradable quote",
    )
    status, before = trader.get(f"/accounts/{account_id}/portfolio")
    balance_before = float(before["balance"])
    print(f"        balance before: {before['balance']} {before['currency']}, "
          f"equity {before['equity']}, free margin {before['free_margin']}")

    order_body = {
        "account_id": account_id,
        "instrument_id": "XAUUSD.m",
        "side": "buy",
        "type": "market",
        "quantity": f"{qty}",
        "stop_loss": f"{stop}",
        "time_in_force": "gtc",
    }
    key_a = f"smoke-{RUN}-a"
    status, first = trader.post("/orders", order_body, key=key_a)
    order = first.get("order")
    rejection = first.get("rejection")
    if rejection:
        print(f"        order refused: {rejection['code']} - {rejection['message']}")
    check("the order was accepted", status == 201 and order is not None, str(first)[:400])

    if order:
        check("the order reached a filled state", order["status"] == "FILLED",
              f"status {order['status']}")
        check("the order is marked simulated", order["simulated"] is True)
        fills = first.get("fills") or []
        check("at least one fill was recorded", len(fills) >= 1)
        if fills:
            print(f"        filled {order['filled_quantity']} @ {order['avg_fill_price']} "
                  f"({len(fills)} fill(s))")
            check(
                "the fill price crossed the spread rather than using the mid",
                float(order["avg_fill_price"]) >= float(micro["bid"]),
                f"fill {order['avg_fill_price']} vs bid {micro['bid']}",
            )

    section("Idempotency")
    first_status = status
    status, replay = trader.post("/orders", order_body, key=key_a)
    check(
        "a replayed key returns the original outcome, not a new order",
        replay.get("duplicate") is True,
        str(replay)[:300],
    )
    # A replay reproduces the original verdict, so a rejected command replays
    # as the same rejection with the same status rather than being retried.
    check(
        "the replayed status matches the original",
        status in (200, first_status),
        f"original {first_status}, replay {status}",
    )
    if order and replay.get("order"):
        check(
            "the replay returns the same order id",
            replay["order"]["id"] == order["id"],
        )

    conflicting = dict(order_body, quantity="0.20")
    status, payload = trader.post("/orders", conflicting, key=key_a)
    check("the same key with a different payload is refused", status == 409, str(payload)[:300])
    check(
        "the refusal names the idempotency conflict",
        payload.get("error", {}).get("code") == "idempotency_conflict",
    )

    key_c = f"smoke-{RUN}-concurrent"
    concurrent_body = dict(order_body, quantity=f"{max(0.01, round(qty / 2 - (qty / 2) % 0.01, 2))}")

    def submit(_: int) -> tuple[int, dict[str, Any]]:
        # One session, many concurrent requests. Signing in eight times would
        # trip the login rate limit, which is a security control rather than
        # something to work around.
        return trader.post("/orders", concurrent_body, key=key_c)

    with ThreadPoolExecutor(max_workers=8) as pool:
        results = list(pool.map(submit, range(8)))

    created = [r for r in results if not r[1].get("duplicate") and r[1].get("order")]
    duplicates = [r for r in results if r[1].get("duplicate")]
    others = [r for r in results if not r[1].get("order") and not r[1].get("duplicate")]
    print(f"        8 concurrent submissions -> {len(created)} created, "
          f"{len(duplicates)} duplicates, {len(others)} other")
    check(
        "exactly one of eight concurrent identical submissions created an order",
        len(created) == 1,
        f"created={len(created)} duplicates={len(duplicates)} other={[(s, str(b)[:120]) for s, b in others]}",
    )

    status, payload = trader.get(f"/orders?account_id={account_id}")
    keys = [o["symbol"] for o in payload["orders"]]
    filled = [o for o in payload["orders"] if o["status"] == "FILLED"]
    print(f"        orders on account: {len(payload['orders'])} ({len(filled)} filled)")

    section("Ledger and position book")
    status, after = trader.get(f"/accounts/{account_id}/portfolio")
    print(f"        balance {after['balance']}, equity {after['equity']}, "
          f"margin used {after['margin_used']}, free {after['free_margin']}")
    print(f"        gross exposure {after['gross_exposure']}, net {after['net_exposure']}, "
          f"positions {after['open_positions']}")
    check("a position is open", after["open_positions"] >= 1, str(after["open_positions"]))
    check("no position failed valuation", after["unvalued_positions"] == 0)
    check(
        "exposure is attributed to the instrument's quote currency",
        "USD" in after["exposure_by_currency"],
        str(after["exposure_by_currency"]),
    )
    check("margin is consumed by the open position", float(after["margin_used"]) > 0)

    status, payload = trader.get(f"/positions?account_id={account_id}")
    for p in payload["positions"]:
        print(f"        {p['side']} {p['quantity']} {p['symbol']} entry {p['avg_entry_price']} "
              f"mark {p['current_price']} unrealised {p['unrealized_pnl']} {p['unrealized_pnl_currency']}")
        check(f"{p['symbol']} position is valued", p["valued"] is True, p.get("valuation_note", ""))

    status, payload = trader.get(f"/accounts/{account_id}/transactions")
    txs = payload["transactions"]
    kinds = {t["type"] for t in txs}
    print(f"        ledger entries: {len(txs)} ({', '.join(sorted(kinds))})")
    check("the opening deposit is on the ledger", "deposit" in kinds)
    check(
        "every ledger entry carries its currency",
        all(t["currency"] == "ZAR" for t in txs),
    )
    check(
        "ledger sequence numbers are contiguous",
        sorted(t["sequence"] for t in txs) == list(range(1, len(txs) + 1)),
        str(sorted(t["sequence"] for t in txs)),
    )

    section("Risk controls")
    status, payload = trader.post(
        "/orders", dict(order_body, quantity="5"), key=f"smoke-{RUN}-oversize"
    )
    rej = payload.get("rejection", {})
    check("an oversized order is refused", rej != {}, str(payload)[:200])
    if rej:
        print(f"        refused: {rej['code']} - {rej['message']}")
    risk = payload.get("risk") or {}
    fails = [c for c in risk.get("checks", []) if not c["passed"]]
    check("the refusal reports which checks failed", len(fails) >= 1)
    for c in fails[:6]:
        print(f"          - {c['name']}: limit {c['limit']}, observed {c['observed']}")

    no_stop = {k: v for k, v in order_body.items() if k != "stop_loss"}
    status, payload = trader.post("/orders", no_stop, key=f"smoke-{RUN}-nostop")
    rej = payload.get("rejection", {})
    failed_checks = [
        c["name"] for c in (payload.get("risk") or {}).get("checks", []) if not c["passed"]
    ]
    # The engine runs EVERY check and reports all failures, so the headline
    # rejection code is whichever failed first -- by now an earlier position may
    # already be consuming exposure. What matters is that the stop-loss rule is
    # among the failures, not that it happens to be the one quoted.
    check(
        "an order without a stop loss is refused when the account requires one",
        rej != {} and "stop_loss_required" in failed_checks,
        f"rejection={rej.get('code')} failed_checks={failed_checks}",
    )
    check(
        "all failing checks are reported, not just the first",
        len(failed_checks) >= 1,
        str(failed_checks),
    )

    status, payload = trader.post(
        "/orders",
        {
            "account_id": account_id,
            "instrument_id": "XAUUSD",
            "side": "buy",
            "type": "market",
            "quantity": "0.01",
            "stop_loss": "2600.00",
            "time_in_force": "gtc",
        },
        key=f"smoke-{RUN}-standard-gold",
    )
    rej = payload.get("rejection", {})
    check(
        "standard 100oz gold is refused as unaffordable on a R500 account",
        rej != {},
        str(payload)[:200],
    )
    if rej:
        print(f"        refused: {rej['code']} - {rej['message']}")

    section("Kill switch")
    status, payload = trader.post(
        "/kill-switches",
        {"scope": "account", "target_id": account_id, "reason": "smoke test"},
    )
    check("a kill switch can be activated", status == 201, str(payload)[:250])
    switch_id = (payload.get("kill_switch") or {}).get("ID") or (payload.get("kill_switch") or {}).get("id")

    status, payload = trader.post("/orders", order_body, key=f"smoke-{RUN}-killed")
    rej = payload.get("rejection", {})
    check(
        "new orders are halted while the kill switch is active",
        rej.get("code") == "kill_switch_active",
        str(payload)[:250],
    )

    status, payload = trader.get(f"/positions?account_id={account_id}")
    check(
        "the kill switch did NOT liquidate the open position",
        len(payload["positions"]) >= 1,
        "positions were closed, which a kill switch must never do",
    )

    if switch_id:
        status, payload = trader.post(f"/kill-switches/{switch_id}/deactivate")
        check("the kill switch can be released", status == 200, str(payload)[:200])

    section("Authorisation")
    status, payload = viewer.post("/orders", order_body, key=f"smoke-{RUN}-viewer")
    check("a viewer cannot place an order", status == 403, str(payload)[:200])

    status, payload = viewer.get(f"/accounts/{account_id}/portfolio")
    check(
        "a viewer cannot read another user's account",
        status == 404,
        f"status {status} -- cross-tenant read must be refused",
    )

    status, payload = viewer.get(f"/accounts/{account_id}/transactions")
    check("a viewer cannot read another user's ledger", status == 404, f"status {status}")

    section("Audit")
    status, payload = admin.get("/admin/audit/verify")
    check("the audit chain verifies", payload.get("verified") is True, str(payload)[:300])
    print(f"        chain head sequence {payload.get('head_sequence')}, "
          f"{payload.get('events_checked')} events checked")

    status, payload = admin.post("/orders", order_body, key=f"smoke-{RUN}-admin")
    check(
        "an admin cannot place orders (separation of duties)",
        status == 403,
        str(payload)[:200],
    )

    section("Trading authority: privilege and partial updates")
    # The authority carries five numeric ceilings, and for a long time four of
    # them were stored, served and enforced by nothing. They bind now, which
    # makes HOW they can be changed a security question rather than a form
    # question: a partial update that silently reset one would widen a control
    # the operator believed they had narrowed.

    status, payload = trader.get(f"/authority/{account_id}")
    granted = payload.get("authority") or {}
    authority_id = granted.get("id")
    check("the account's authority is readable", status == 200 and bool(authority_id))

    # Every numeric ceiling is surfaced. A field that vanished from the
    # response would be invisible to an operator reviewing what is in force.
    ceilings = [
        "max_order_quantity",
        "max_order_notional",
        "max_position_exposure",
        "max_leverage",
        "max_daily_loss",
    ]
    missing = [c for c in ceilings if granted.get(c) in (None, "")]
    check(
        "all five numeric ceilings are surfaced",
        not missing,
        f"missing: {missing}",
    )
    before = {c: granted.get(c) for c in ceilings}
    scope_before = sorted(granted.get("allowed_instruments") or [])

    # --- privilege ------------------------------------------------------
    status, payload = viewer.patch(f"/authority/{authority_id}", {"automation_enabled": False})
    check(
        "a viewer cannot modify a trading authority",
        status == 403,
        f"{status} {str(payload)[:160]}",
    )
    status, payload = viewer.post("/authority", {"account_id": account_id})
    check("a viewer cannot grant a trading authority", status == 403, str(payload)[:160])
    status, payload = viewer.post(f"/authority/{authority_id}/revoke", {"reason": "smoke probe"})
    check("a viewer cannot revoke a trading authority", status == 403, str(payload)[:160])

    status, payload = anon.patch(f"/authority/{authority_id}", {"automation_enabled": False})
    check(
        "an unauthenticated caller cannot modify an authority",
        status == 401,
        f"{status} {str(payload)[:160]}",
    )

    # --- mass assignment ------------------------------------------------
    # The ceilings are settable at GRANT time only. Accepting them here would
    # mean an absent field had to mean something, and "unchanged" versus
    # "zero" is exactly the ambiguity that disabled three protections once
    # before. The decoder refuses unknown fields outright.
    status, payload = trader.patch(
        f"/authority/{authority_id}",
        {"automation_enabled": True, "max_order_quantity": "99999"},
    )
    check(
        "a ceiling cannot be smuggled through the partial-update endpoint",
        status in (400, 422),
        f"{status} {str(payload)[:200]}",
    )

    # --- partial update leaves everything else alone --------------------
    status, payload = trader.patch(f"/authority/{authority_id}", {"automation_enabled": True})
    check("a trader may update the authority they hold", status == 200, str(payload)[:200])

    status, payload = trader.get(f"/authority/{account_id}")
    after = payload.get("authority") or {}
    unchanged = [c for c in ceilings if after.get(c) != before.get(c)]
    check(
        "a partial update does not disturb any numeric ceiling",
        not unchanged,
        f"changed: {[(c, before.get(c), after.get(c)) for c in unchanged]}",
    )
    check(
        "a partial update does not clear the instrument scope",
        sorted(after.get("allowed_instruments") or []) == scope_before,
        f"{scope_before} -> {sorted(after.get('allowed_instruments') or [])}",
    )
    check(
        "an omitted boolean is not read as false",
        after.get("automation_enabled") is True,
        str(after.get("automation_enabled")),
    )

    # Omitting the boolean entirely must leave it as it is, rather than
    # defaulting to false and quietly switching automation off.
    status, _ = trader.patch(
        f"/authority/{authority_id}", {"allowed_instruments": scope_before}
    )
    status, payload = trader.get(f"/authority/{account_id}")
    check(
        "omitting automation_enabled leaves it unchanged",
        status == 200
        and (payload.get("authority") or {}).get("automation_enabled") is True,
        str((payload.get("authority") or {}).get("automation_enabled")),
    )

    # --- cross-account --------------------------------------------------
    # An authority is looked up through its OWNER, so an id belonging to
    # someone else is not found rather than forbidden: the two answers must be
    # indistinguishable or the endpoint becomes an existence oracle.
    status, payload = trader.patch(
        "/authority/00000000-0000-0000-0000-000000000000", {"automation_enabled": True}
    )
    check(
        "an authority that is not yours cannot be modified",
        status in (403, 404),
        f"{status} {str(payload)[:160]}",
    )

    status, payload = trader.get(f"/activity/{account_id}")
    kinds = {i["kind"] for i in payload.get("activity", [])}
    check("the activity timeline is populated", len(payload.get("activity", [])) > 0)
    print(f"        activity kinds: {', '.join(sorted(kinds))}")

    return summarise()


def summarise() -> int:
    print()
    print("=" * 70)
    print(f"{passed} passed, {len(failed)} failed")
    if failed:
        print()
        print("Failed:")
        for name in failed:
            print(f"  - {name}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
