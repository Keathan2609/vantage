/**
 * Typed client for the Vantage control-plane API.
 *
 * Design notes:
 *
 * - The session lives in an HttpOnly cookie the browser sends automatically.
 *   No token is ever held in JavaScript, so an XSS bug in this app cannot
 *   exfiltrate a session.
 * - Every state-changing request echoes the readable CSRF cookie into a header.
 *   The pair -- an HttpOnly session cookie plus a header only same-origin script
 *   can set -- is what makes a cross-site request fail.
 * - Money and prices arrive as STRINGS and stay strings. Parsing "0.1" into a
 *   float to render it would reintroduce the precision problem the backend
 *   went to some trouble to avoid. Numbers are formatted, not computed, here.
 * - Errors carry a stable machine-readable code. UI branches on the code and
 *   shows the server's message, rather than inventing its own wording for a
 *   condition the server understands better.
 */

const BASE =
  process.env.NEXT_PUBLIC_VANTAGE_API_BASE_URL ?? "http://localhost:8080";

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly detail?: Record<string, string>;
  readonly requestId?: string;

  constructor(
    status: number,
    code: string,
    message: string,
    detail?: Record<string, string>,
    requestId?: string,
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.detail = detail;
    this.requestId = requestId;
  }

  /** True when signing in again is what the user needs to do. */
  get isAuthFailure(): boolean {
    return this.status === 401;
  }
}

function readCookie(name: string): string | null {
  if (typeof document === "undefined") return null;
  const match = document.cookie
    .split("; ")
    .find((row) => row.startsWith(`${name}=`));
  return match ? decodeURIComponent(match.slice(name.length + 1)) : null;
}

function csrfToken(): string | null {
  // The __Host- prefixed name is used when served over HTTPS.
  return readCookie("vantage_csrf") ?? readCookie("__Host-vantage_csrf");
}

interface RequestOptions {
  method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  body?: unknown;
  idempotencyKey?: string;
  signal?: AbortSignal;
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? "GET";
  const headers: Record<string, string> = { Accept: "application/json" };

  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (method !== "GET") {
    const token = csrfToken();
    if (token) headers["X-Vantage-CSRF"] = token;
  }
  if (options.idempotencyKey) {
    headers["Idempotency-Key"] = options.idempotencyKey;
  }

  const response = await fetch(`${BASE}${path}`, {
    method,
    headers,
    // Sends the HttpOnly session cookie cross-origin. The server allows
    // exactly one origin with credentials; it never reflects the request's.
    credentials: "include",
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    signal: options.signal,
  });

  const text = await response.text();
  const payload: unknown = text ? JSON.parse(text) : {};

  if (!response.ok) {
    const envelope = payload as {
      error?: {
        code?: string;
        message?: string;
        detail?: Record<string, string>;
        request_id?: string;
      };
    };
    throw new ApiError(
      response.status,
      envelope.error?.code ?? "unknown_error",
      envelope.error?.message ?? `Request failed with status ${response.status}`,
      envelope.error?.detail,
      envelope.error?.request_id,
    );
  }
  return payload as T;
}

/** A short, unique idempotency key for one user action. */
export function newIdempotencyKey(prefix: string): string {
  const random =
    typeof crypto !== "undefined" && "randomUUID" in crypto
      ? crypto.randomUUID()
      : `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  return `${prefix}-${random}`;
}

/* ------------------------------------------------------------------ types */

export interface User {
  id: string;
  email: string;
  display_name: string;
  role: "admin" | "trader" | "viewer";
  mfa_enabled: boolean;
}

export interface Session {
  user: User;
  execution_mode: string;
  simulated_funds: boolean;
  mfa_satisfied: boolean;
  expires_at: string;
}

export interface VersionInfo {
  version: string;
  commit: string;
  execution_mode: string;
  simulated_funds: boolean;
  live_trading_available: boolean;
}

export interface Account {
  id: string;
  name: string;
  mode: string;
  currency: string;
  broker_name: string;
  enabled: boolean;
  trading_enabled: boolean;
  leverage: string;
  simulated: boolean;
  version: number;
  created_at: string;
}

export interface Position {
  id: string;
  account_id: string;
  instrument_id: string;
  symbol: string;
  side: "buy" | "sell";
  quantity: string;
  avg_entry_price: string;
  current_price: string;
  stop_loss?: string;
  take_profit?: string;
  unrealized_pnl: string;
  unrealized_pnl_currency: string;
  notional_value: string;
  margin_used: string;
  valued: boolean;
  valuation_note?: string;
  strategy_id?: string;
  opened_at: string;
  simulated: boolean;
}

export interface Portfolio {
  account: Account;
  balance: string;
  equity: string;
  margin_used: string;
  free_margin: string;
  margin_level_pct: string;
  realized_pnl: string;
  unrealized_pnl: string;
  day_pnl: string;
  commission: string;
  swap: string;
  gross_exposure: string;
  net_exposure: string;
  peak_equity: string;
  drawdown_fraction: string;
  open_positions: number;
  pending_orders: number;
  currency: string;
  positions: Position[];
  exposure_by_instrument: Record<string, string>;
  exposure_by_currency: Record<string, string>;
  unvalued_positions: number;
  simulated: boolean;
  as_of: string;
}

export interface Order {
  id: string;
  account_id: string;
  instrument_id: string;
  symbol: string;
  mode: string;
  simulated: boolean;
  side: "buy" | "sell";
  type: string;
  time_in_force: string;
  status: string;
  quantity: string;
  filled_quantity: string;
  avg_fill_price: string;
  limit_price?: string;
  stop_price?: string;
  stop_loss?: string;
  take_profit?: string;
  source: string;
  strategy_id?: string;
  broker_name: string;
  broker_order_id?: string;
  reject_code?: string;
  reject_reason?: string;
  version: number;
  created_at: string;
  updated_at: string;
  submitted_at?: string;
  closed_at?: string;
}

export interface Fill {
  id: string;
  quantity: string;
  price: string;
  commission: string;
  commission_currency: string;
  executed_at: string;
}

export interface RiskCheck {
  name: string;
  passed: boolean;
  limit: string;
  observed: string;
  message: string;
}

export interface Rejection {
  code: string;
  message: string;
  detail?: Record<string, string>;
}

export interface PlaceOrderResult {
  order?: Order;
  fills?: Fill[];
  rejection?: Rejection;
  risk?: { approved: boolean; approved_quantity: string; checks: RiskCheck[] };
  duplicate: boolean;
  simulated: boolean;
}

export interface Instrument {
  id: string;
  symbol: string;
  name: string;
  asset_class: string;
  base_currency: string;
  quote_currency: string;
  enabled: boolean;
  contract_size: string;
  price_precision: number;
  tick_size: string;
  min_quantity: string;
  max_quantity: string;
  quantity_step: string;
  margin_rate: string;
  max_leverage: string;
  supported_order_types: string[];
  commission_per_lot: string;
}

export interface Quote {
  instrument_id: string;
  symbol: string;
  bid: string;
  ask: string;
  mid: string;
  spread: string;
  spread_fraction: string;
  source_time: string;
  ingested_at: string;
  age_seconds: number;
  provider: string;
  health: string;
  tradable_by_automation: boolean;
}

export interface MarketHealth {
  instrument_id: string;
  symbol: string;
  state: string;
  issues: string[];
  quote_age_ms: number;
  spread_fraction: string;
  provider: string;
  tradable_by_automation: boolean;
}

export interface MarketStatus {
  status: string;
  tradable: boolean;
  active_sessions: string[];
  server_time: string;
  next_close?: string;
}

export interface Bar {
  open_time: string;
  open: string;
  high: string;
  low: string;
  close: string;
  volume: string;
}

export interface RiskLimits {
  account_id: string;
  currency: string;
  max_order_quantity: string;
  max_order_notional: string;
  max_risk_per_trade_fraction: string;
  require_stop_loss: boolean;
  max_open_positions: number;
  max_pending_orders: number;
  max_gross_exposure: string;
  max_net_exposure: string;
  max_per_instrument_exposure: string;
  max_concentration_fraction: string;
  max_leverage: string;
  max_daily_loss: string;
  max_drawdown_fraction: string;
  max_spread_fraction: string;
  max_slippage_fraction: string;
  event_blackout_before_minutes: number;
  event_blackout_after_minutes: number;
  block_on_high_impact_events: boolean;
  version: number;
  updated_at: string;
}

export interface Authority {
  id: string;
  account_id: string;
  mode: string;
  active: boolean;
  automation_enabled: boolean;
  allowed_instruments: string[];
  allowed_strategy_ids: string[];
  allowed_order_types: string[];
  max_order_quantity: string;
  max_order_notional: string;
  max_position_exposure: string;
  max_leverage: string;
  max_daily_loss: string;
  currency: string;
  valid_from: string;
  valid_until?: string;
  revoked_at?: string;
  revocation_reason?: string;
  version: number;
  notice: string;
}

export interface KillSwitch {
  ID: string;
  Scope: string;
  TargetID?: string;
  Active: boolean;
  Reason: string;
  ActivatedAt: string;
  DeactivatedAt?: string;
}

export interface StrategyRow {
  id: string;
  key: string;
  name: string;
  family: string;
  description: string;
  high_risk: boolean;
  enabled: boolean;
  lifecycle: string;
  version: number;
  timeframe: string;
  instruments: string[] | null;
  runs_7d: number;
  signals_7d: number;
  failures_7d: number;
  last_run_at?: string;
  last_signal_at?: string;
}

export interface StrategySignal {
  ID: string;
  StrategyID: string;
  StrategyVersion: number;
  InstrumentID: string;
  Timeframe: string;
  Action: string;
  Confidence: string;
  SuggestedStop?: string;
  SuggestedTarget?: string;
  Explanation: string;
  BarTime: string;
  GeneratedAt: string;
}

export interface Backtest {
  ID: string;
  StrategyID: string;
  StrategyVersion: number;
  InstrumentID: string;
  Timeframe: string;
  DatasetHash: string;
  PeriodStart: string;
  PeriodEnd: string;
  SampleKind: string;
  InitialCapital: string;
  Currency: string;
  Frictionless: boolean;
  Metrics: Record<string, unknown>;
  Warnings: string[] | null;
  Status: string;
  StartedAt: string;
}

export interface ModelVersion {
  ID: string;
  ModelID: string;
  Version: number;
  Algorithm: string;
  DatasetHash: string;
  CodeGitSHA: string;
  RandomSeed: number;
  Lifecycle: string;
  TrainStart: string;
  TrainEnd: string;
  ValidationStart?: string;
  ValidationEnd?: string;
  TestStart?: string;
  TestEnd?: string;
  CreatedAt: string;
  ModelKey: string;
  ModelName: string;
  Task: string;
  evaluations?: Array<{
    Split: string;
    Metrics: Record<string, unknown>;
    PeriodStart: string;
    PeriodEnd: string;
  }>;
}

export interface EconomicEvent {
  ID: string;
  ScheduledAt: string;
  Country: string;
  Currency: string;
  Impact: "low" | "medium" | "high";
  EventName: string;
  Actual?: string;
  Forecast?: string;
  Previous?: string;
  Revised?: string;
  Status: string;
  Source: string;
}

export interface NewsItem {
  ID: string;
  Source: string;
  Headline: string;
  Summary?: string;
  PublishedAt: string;
  RelatedInstruments: string[] | null;
  RelatedCurrencies: string[] | null;
  Sentiment?: string;
  Topics: string[] | null;
}

export interface ScanCandidate {
  instrument_id: string;
  symbol: string;
  regime: string;
  trend_score: string;
  momentum_score: string;
  volatility: string;
  spread_fraction: string;
  event_risk: string;
  score: string;
  agreeing_strategies: string[];
  explanation: string;
  indicators: Record<string, string>;
}

export interface ActivityItem {
  kind: string;
  title: string;
  detail: string;
  severity: string;
  reference?: string;
  at: string;
}

export interface Transaction {
  id: string;
  sequence: number;
  type: string;
  amount: string;
  balance_after: string;
  currency: string;
  description: string;
  order_id?: string;
  created_at: string;
}

export interface EquityPoint {
  equity: string;
  balance: string;
  unrealized_pnl: string;
  margin_used: string;
  recorded_at: string;
}

export interface RiskEvent {
  ID: string;
  Severity: string;
  Check: string;
  Code: string;
  Message: string;
  InstrumentID?: string;
  CreatedAt: string;
}

export interface Connection {
  name: string;
  kind: string;
  mode: string;
  status: string;
  detail?: string;
  latency_ms?: number;
}

export interface SessionSummary {
  id: string;
  ip_address?: string;
  user_agent?: string;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
  current: boolean;
}

export interface AuditEvent {
  Sequence: number;
  OccurredAt: string;
  ActorType: string;
  Action: string;
  TargetType: string;
  Result: string;
  Hash: string;
}

export interface Decision {
  ID: string;
  InstrumentID: string;
  StrategyID?: string;
  SignalAction: string;
  Confidence?: string;
  RequestedQty?: string;
  ApprovedQty?: string;
  Outcome: string;
  OutcomeCode?: string;
  OutcomeReason?: string;
  CreatedAt: string;
}

export interface ReconciliationIssue {
  id: string;
  run_id: string;
  account_id: string;
  broker_name: string;
  issue_type: string;
  severity: "info" | "warning" | "critical";
  status:
    | "OPEN"
    | "AUTOMATICALLY_REPAIRED"
    | "OPERATOR_ACTION_REQUIRED"
    | "RESOLVED"
    | "UNRESOLVABLE";
  repair_class:
    | "AUTOMATICALLY_SAFE"
    | "OPERATOR_REVIEW_REQUIRED"
    | "UNRESOLVABLE_AUTOMATICALLY";
  /**
   * Sent by the server rather than derived here. The repair policy is enforced
   * server-side, and a terminal that re-implemented it would eventually offer
   * an action the server refuses -- which reads to an operator as a broken
   * button rather than as a rule.
   */
  automatic_repair_allowed: boolean;
  halt_scope: "NONE" | "ACCOUNT" | "BROKER_CONNECTION" | "ALL";
  rationale: string;
  allowed_actions: string[];
  order_id: string | null;
  position_id: string | null;
  instrument_id: string | null;
  broker_order_id: string | null;
  broker_execution_id: string | null;
  local_state: unknown;
  broker_state: unknown;
  evidence: unknown;
  description: string;
  detected_at: string;
  last_checked_at: string;
  check_count: number;
  resolved_at: string | null;
  resolution_action: string | null;
  resolution_reason: string | null;
}

export type TradingState =
  | "HEALTHY"
  | "DEGRADED"
  | "RECONCILIATION_REQUIRED"
  | "TRADING_HALTED";

export interface ReconciliationIssues {
  account_id: string;
  trading_state: TradingState;
  automation_allowed: boolean;
  halt_scope: string;
  reason: string;
  open_issues: number;
  critical_issues: number;
  operator_action_required: number;
  uncertain_orders: number;
  consecutive_failures: number;
  last_success_at: string | null;
  issues: ReconciliationIssue[] | null;
}

export interface OperationsAccount {
  account_id: string;
  broker_name: string;
  trading_state: TradingState;
  automation_allowed: boolean;
  halt_scope: string;
  reason: string;
  open_issues: number;
  critical_issues: number;
  operator_action_required: number;
  uncertain_orders: number;
  last_success_at: string | null;
  consecutive_failures: number;
}

export interface OperationsOverview {
  trading_state: TradingState;
  halted_accounts: number;
  open_issues: number;
  critical_issues: number;
  accounts: OperationsAccount[] | null;
  execution_mode: string;
}

export interface IssueEvent {
  action: string;
  from_status: string | null;
  to_status: string;
  actor_type: string;
  reason: string;
  occurred_at: string;
}

export interface ReconciliationStatus {
  automation_blocked: boolean;
  critical_discrepancies: number;
  unresolved: unknown[] | null;
  last_run?: {
    Trigger: string;
    Status: string;
    OrdersCompared: number;
    PositionsCompared: number;
    Discrepancies: number;
    StartedAt: string;
    FinishedAt?: string;
  };
}

export interface AdminUser extends User {
  disabled: boolean;
  last_login_at?: string;
  created_at: string;
}

export interface AuditVerification {
  verified: boolean;
  events_checked: number;
  head_sequence: number;
  head_hash: string;
  note: string;
  broken_at_index?: number;
  broken_at_sequence?: number;
}

export interface MfaEnrolment {
  secret: string;
  provisioning_uri: string;
  recovery_codes: string[];
  message: string;
}

/* ------------------------------------------------------------------- calls */

export type AttributionDimension =
  | "instrument"
  | "strategy"
  | "strategy_version"
  | "source"
  | "session"
  | "event_context"
  | "type"
  | "run_kind"
  | "replay_run"
  | "regime";

export interface AttributionBucket {
  key: string;
  label: string;
  // Gross, costs and net are separate on purpose: a strategy profitable
  // before costs and losing after them is the finding worth surfacing.
  gross_pnl: string;
  costs: string;
  net_pnl: string;
  other: string;
  trades: number;
  wins: number;
  win_rate: string;
  entries: number;
}

export interface ProviderHealth {
  provider: string;
  state:
    | "CONNECTED"
    | "DEGRADED"
    | "RATE_LIMITED"
    | "UNAVAILABLE"
    | "MISCONFIGURED";
  configured: boolean;
  last_success_at?: string;
  last_failure_at?: string;
  last_failure_reason?: string;
  last_latency_ms?: number;
  last_market_timestamp?: string;
  requests_this_minute: number;
  requests_per_minute: number;
}

export interface MarketDataCoverage {
  instrument_id: string;
  timeframe: string;
  bars: number;
  earliest_bar?: string;
  latest_bar?: string;
  providers: string[];
  last_sync_at?: string;
  last_sync_kind?: string;
  last_sync_status?: string;
}

export interface MarketDataGap {
  from: string;
  to: string;
  missing_bars: number;
}

export interface MarketDataSegment {
  id: string;
  kind: string;
  status: string;
  requested_start: string;
  requested_end: string;
  received_start?: string;
  received_end?: string;
  rows_returned: number;
  rows_stored: number;
  rows_duplicate: number;
  rows_invalid: number;
  provider: string;
  provider_symbol: string;
  failure_reason?: string;
  warnings: string[];
}

export const api = {
  // Auth
  version: () => request<VersionInfo>("/version"),
  login: (email: string, password: string) =>
    request<{ user: User; mfa_required: boolean; csrf_token: string; expires_at: string }>(
      "/api/v1/auth/login",
      { method: "POST", body: { email, password } },
    ),
  logout: () => request<{ status: string }>("/api/v1/auth/logout", { method: "POST" }),
  session: () => request<Session>("/api/v1/auth/session"),
  sessions: () => request<{ sessions: SessionSummary[] }>("/api/v1/auth/sessions"),
  revokeSession: (id: string) =>
    request<{ status: string }>(`/api/v1/auth/sessions/${id}/revoke`, { method: "POST" }),
  verifyMfa: (code: string) =>
    request<{ status: string; expires_at?: string }>("/api/v1/auth/mfa/verify", {
      method: "POST",
      body: { code },
    }),
  changePassword: (currentPassword: string, newPassword: string) =>
    request<{ status: string; message: string }>("/api/v1/auth/password", {
      method: "POST",
      body: { current_password: currentPassword, new_password: newPassword },
    }),
  enrollMfa: () => request<MfaEnrolment>("/api/v1/auth/mfa/enroll", { method: "POST" }),
  activateMfa: (code: string) =>
    request<{ status: string }>("/api/v1/auth/mfa/activate", { method: "POST", body: { code } }),
  disableMfa: (password: string, code: string) =>
    request<{ status: string }>("/api/v1/auth/mfa/disable", {
      method: "POST",
      body: { password, code },
    }),

  // Accounts and portfolio
  accounts: () => request<{ accounts: Account[] }>("/api/v1/accounts"),
  portfolio: (accountId: string) =>
    request<Portfolio>(`/api/v1/accounts/${accountId}/portfolio`),
  equityHistory: (accountId: string, limit = 500) =>
    request<{ points: EquityPoint[]; currency: string }>(
      `/api/v1/accounts/${accountId}/equity-history?limit=${limit}`,
    ),
  transactions: (accountId: string, limit = 100) =>
    request<{ transactions: Transaction[] }>(
      `/api/v1/accounts/${accountId}/transactions?limit=${limit}`,
    ),
  attribution: (accountId: string, by: AttributionDimension = "instrument") =>
    request<{
      dimension: AttributionDimension;
      dimensions: AttributionDimension[];
      attribution: AttributionBucket[];
      total: AttributionBucket;
      // False when the buckets do not account for every ledger entry the
      // account has, which means the figures describe a window rather than
      // the account. Shown to the operator rather than hidden.
      reconciled: boolean;
      ledger_entries: number;
      ledger_net: string;
      discrepancy: string;
      currency: string;
    }>(`/api/v1/accounts/${accountId}/attribution?by=${by}`),

  // Market data
  marketDataStatus: () =>
    request<{ provider: ProviderHealth }>(`/api/v1/market-data/status`),

  marketDataCoverage: (instrumentId: string, timeframe: string) =>
    request<{
      coverage: MarketDataCoverage;
      gaps: MarketDataGap[];
      segments: MarketDataSegment[];
    }>(
      `/api/v1/market-data/coverage?instrument_id=${encodeURIComponent(instrumentId)}&timeframe=${timeframe}`,
    ),

  instruments: (enabledOnly = false) =>
    request<{ instruments: Instrument[] }>(
      `/api/v1/instruments${enabledOnly ? "?enabled=true" : ""}`,
    ),
  quotes: () => request<{ quotes: Quote[]; as_of: string }>("/api/v1/market/quotes"),
  marketHealth: () => request<{ health: MarketHealth[] }>("/api/v1/market/health"),
  marketStatus: () => request<MarketStatus>("/api/v1/market/status"),
  bars: (instrumentId: string, timeframe: string, limit = 400, endBefore?: string) =>
    request<{ instrument_id: string; timeframe: string; bars: Bar[] }>(
      `/api/v1/market/bars?instrument_id=${encodeURIComponent(instrumentId)}&timeframe=${timeframe}&limit=${limit}` +
        (endBefore ? `&end=${encodeURIComponent(endBefore)}` : ""),
    ),

  // Trading
  orders: (params: { accountId?: string; openOnly?: boolean; limit?: number } = {}) => {
    const search = new URLSearchParams();
    if (params.accountId) search.set("account_id", params.accountId);
    if (params.openOnly) search.set("open", "true");
    search.set("limit", String(params.limit ?? 100));
    return request<{ orders: Order[]; simulated: boolean }>(`/api/v1/orders?${search}`);
  },
  order: (id: string) =>
    request<{
      order: Order;
      fills: Fill[];
      transitions: Array<{
        FromStatus?: string;
        ToStatus: string;
        Reason?: string;
        ActorType: string;
        OccurredAt: string;
      }>;
    }>(`/api/v1/orders/${id}`),
  placeOrder: (
    body: {
      account_id: string;
      instrument_id: string;
      side: "buy" | "sell";
      type: string;
      quantity: string;
      limit_price?: string;
      stop_price?: string;
      stop_loss?: string;
      take_profit?: string;
      time_in_force: string;
    },
    idempotencyKey: string,
  ) =>
    request<PlaceOrderResult>("/api/v1/orders", {
      method: "POST",
      body,
      idempotencyKey,
    }),
  cancelOrder: (id: string) =>
    request<{ order: Order }>(`/api/v1/orders/${id}/cancel`, { method: "POST" }),
  positions: (accountId?: string) =>
    request<{ positions: Position[]; simulated: boolean }>(
      `/api/v1/positions${accountId ? `?account_id=${accountId}` : ""}`,
    ),
  flatten: (positionId: string, idempotencyKey: string) =>
    request<PlaceOrderResult>(`/api/v1/positions/${positionId}/flatten`, {
      method: "POST",
      body: { confirm: true },
      idempotencyKey,
    }),

  // Risk and control
  riskLimits: (accountId: string) =>
    request<{ limits: RiskLimits; utilisation: Record<string, string>; simulated: boolean }>(
      `/api/v1/risk/limits/${accountId}`,
    ),
  riskEvents: (accountId: string, limit = 50) =>
    request<{ events: RiskEvent[] | null }>(`/api/v1/risk/events/${accountId}?limit=${limit}`),
  authority: (accountId: string) =>
    request<{ authority: Authority | null; notice?: string; message?: string }>(
      `/api/v1/authority/${accountId}`,
    ),
  killSwitches: (activeOnly = false) =>
    request<{ kill_switches: KillSwitch[] | null }>(
      `/api/v1/kill-switches${activeOnly ? "?active=true" : ""}`,
    ),
  activateKillSwitch: (scope: string, targetId: string | null, reason: string) =>
    request<{ kill_switch: KillSwitch; message: string }>("/api/v1/kill-switches", {
      method: "POST",
      body: { scope, target_id: targetId, reason },
    }),
  deactivateKillSwitch: (id: string) =>
    request<{ status: string }>(`/api/v1/kill-switches/${id}/deactivate`, { method: "POST" }),
  createAuthority: (body: {
    account_id: string;
    automation_enabled: boolean;
    allowed_instruments: string[];
    allowed_strategy_ids: string[];
    allowed_order_types: string[];
    max_order_quantity: string;
    max_order_notional: string;
    max_position_exposure: string;
    max_leverage: string;
    max_daily_loss: string;
    valid_until?: string;
  }) => request<{ authority: Authority }>("/api/v1/authority", { method: "POST", body }),
  updateAuthority: (
    id: string,
    body: {
      automation_enabled?: boolean;
      allowed_instruments?: string[];
      allowed_strategy_ids?: string[];
      version: number;
    },
  ) => request<{ authority: Authority }>(`/api/v1/authority/${id}`, { method: "PATCH", body }),
  revokeAuthority: (id: string, reason: string) =>
    request<{ status: string; message: string }>(`/api/v1/authority/${id}/revoke`, {
      method: "POST",
      body: { reason },
    }),
  setTradingEnabled: (accountId: string, enabled: boolean, reason: string) =>
    request<{ account_id: string; trading_enabled: boolean }>(
      `/api/v1/accounts/${accountId}/trading-enabled`,
      { method: "POST", body: { enabled, reason } },
    ),
  updateRiskLimits: (accountId: string, body: Record<string, unknown>) =>
    request<{ limits: RiskLimits }>(`/api/v1/risk/limits/${accountId}`, {
      method: "PUT",
      body,
    }),
  reconciliation: (accountId: string) =>
    request<ReconciliationStatus>(`/api/v1/reconciliation/${accountId}`),
  runReconciliation: (accountId: string) =>
    request<{
      status: string;
      orders_compared: number;
      positions_compared: number;
      executions_seen: number;
      repaired: number;
      clean: boolean;
      critical: number;
      issues: ReconciliationIssue[] | null;
    }>(`/api/v1/reconciliation/${accountId}/run`, { method: "POST" }),
  reconciliationIssues: (accountId: string, openOnly = false) =>
    request<ReconciliationIssues>(
      `/api/v1/reconciliation/${accountId}/issues${openOnly ? "?open=true" : ""}`,
    ),
  reconciliationIssue: (accountId: string, issueId: string) =>
    request<{ issue: ReconciliationIssue; history: IssueEvent[] | null }>(
      `/api/v1/reconciliation/${accountId}/issues/${issueId}`,
    ),
  operations: () => request<OperationsOverview>("/api/v1/operations"),
  /**
   * Resolve a reconciliation issue. ADMIN only, enforced server-side.
   *
   * Note what is NOT in this signature: no quantity, no price, no target
   * status. Every number involved comes from the issue's own recorded
   * evidence. A caller able to supply them would be writing arbitrary values
   * into an append-only ledger with an operator's authority attached.
   */
  resolveReconciliationIssue: (
    accountId: string,
    issueId: string,
    body: { action: string; reason: string; broker_order_id?: string },
  ) =>
    request<{
      issue: ReconciliationIssue;
      detail: string;
      state_changed: boolean;
    }>(`/api/v1/reconciliation/${accountId}/issues/${issueId}/resolve`, {
      method: "POST",
      body,
    }),

  // Research
  strategies: () =>
    request<{ strategies: StrategyRow[]; note: string }>("/api/v1/strategies"),
  runStrategy: (
    strategyId: string,
    body: { account_id: string; instrument_id: string; version: number; dry_run: boolean },
  ) =>
    request<{
      status: string;
      action: string;
      confidence: string;
      explanation: string;
      skip_reason?: string;
      executed: boolean;
      order?: Order;
      rejection?: Rejection;
      indicators?: Record<string, string>;
    }>(`/api/v1/strategies/${strategyId}/run`, { method: "POST", body }),
  signals: (accountId?: string, limit = 25) =>
    request<{ signals: StrategySignal[] | null }>(
      `/api/v1/signals?limit=${limit}${accountId ? `&account_id=${accountId}` : ""}`,
    ),
  backtests: (strategyId?: string) =>
    request<{ backtests: Backtest[] | null }>(
      `/api/v1/backtests${strategyId ? `?strategy_id=${strategyId}` : ""}`,
    ),
  runBacktest: (body: {
    strategy_id: string;
    version: number;
    instrument_id: string;
    timeframe: string;
    from: string;
    to: string;
    initial_capital: string;
    currency: string;
    sample_kind: string;
    risk_per_trade: string;
  }) =>
    request<{
      backtest_id: string;
      metrics: Record<string, unknown>;
      trades: number;
      warnings: string[];
      sample_kind: string;
      note: string;
    }>("/api/v1/backtests", { method: "POST", body }),
  models: () => request<{ models: ModelVersion[] | null; note: string }>("/api/v1/ml/models"),
  trainModel: (body: {
    model_key: string;
    algorithm: string;
    instrument_id: string;
    timeframe: string;
    from: string;
    to: string;
    horizon: number;
    seed: number;
  }) =>
    request<{
      model_version_id: string;
      version: number;
      algorithm: string;
      evaluations: Record<string, Record<string, unknown>>;
      windows: Record<string, Record<string, unknown>>;
      warnings: string[];
      lifecycle: string;
      dataset_hash: string;
      note: string;
    }>("/api/v1/ml/train", { method: "POST", body }),
  scanner: (timeframe = "1h") =>
    request<{ candidates: ScanCandidate[]; timeframe: string; note: string }>(
      `/api/v1/scanner?timeframe=${timeframe}`,
    ),
  calendar: (range = "week", impact?: string) =>
    request<{ events: EconomicEvent[] | null; note: string }>(
      `/api/v1/calendar?range=${range}${impact ? `&impact=${impact}` : ""}`,
    ),
  news: (instrumentId?: string) =>
    request<{ news: NewsItem[] | null; note: string }>(
      `/api/v1/news${instrumentId ? `?instrument_id=${instrumentId}` : ""}`,
    ),
  decisions: (accountId: string, limit = 50) =>
    request<{ decisions: Decision[] | null }>(
      `/api/v1/decisions/${accountId}?limit=${limit}`,
    ),

  // Operations
  activity: (accountId: string, limit = 60) =>
    request<{ activity: ActivityItem[] | null }>(
      `/api/v1/activity/${accountId}?limit=${limit}`,
    ),
  audit: (limit = 100) => request<{ events: AuditEvent[] | null }>(`/api/v1/audit?limit=${limit}`),
  connections: () => request<{ connections: Connection[] }>("/api/v1/connections"),
  adminUsers: () => request<{ users: AdminUser[] }>("/api/v1/admin/users"),
  verifyAuditChain: () => request<AuditVerification>("/api/v1/admin/audit/verify"),
  notifications: (unreadOnly = false) =>
    request<{
      notifications: Array<{
        ID: string;
        Severity: string;
        Category: string;
        Title: string;
        Body: string;
        ReadAt?: string;
        CreatedAt: string;
      }> | null;
    }>(`/api/v1/notifications${unreadOnly ? "?unread=true" : ""}`),
};
