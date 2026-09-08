/**
 * Display formatting.
 *
 * Money arrives from the API as a decimal STRING and is formatted here without
 * ever becoming a float. `Number("0.1") + Number("0.2")` is the reason: the
 * backend carries exact decimals through the ledger, and parsing them for
 * display would be the one place that precision quietly disappears.
 *
 * Currency is never omitted. A bare "500" in a financial interface is
 * ambiguous, and the account here is denominated in rand while its instrument
 * is quoted in dollars.
 */

/** Format a decimal string to a fixed number of places, without float maths. */
export function decimal(value: string | undefined | null, places = 2): string {
  if (value === undefined || value === null || value === "") return "—";

  const negative = value.startsWith("-");
  const unsigned = negative ? value.slice(1) : value;
  const [whole = "0", fraction = ""] = unsigned.split(".");

  let digits = fraction;
  if (digits.length > places) {
    // Round half-up on the string itself, so no float is ever involved.
    const keep = digits.slice(0, places);
    const next = digits.charCodeAt(places) - 48;
    if (next >= 5) {
      const bumped = (BigInt(whole + (keep || "")) + 1n).toString().padStart(
        whole.length + places,
        "0",
      );
      const splitAt = bumped.length - places;
      const roundedWhole = places === 0 ? bumped : bumped.slice(0, splitAt) || "0";
      const roundedFraction = places === 0 ? "" : bumped.slice(splitAt);
      return (
        (negative ? "-" : "") +
        group(roundedWhole) +
        (roundedFraction ? `.${roundedFraction}` : "")
      );
    }
    digits = keep;
  } else {
    digits = digits.padEnd(places, "0");
  }

  return (negative ? "-" : "") + group(whole) + (places > 0 ? `.${digits}` : "");
}

function group(whole: string): string {
  return whole.replace(/\B(?=(\d{3})+(?!\d))/g, " ");
}

/** Money with its currency. Never one without the other. */
export function money(
  value: string | undefined | null,
  currency: string,
  places = 2,
): string {
  if (value === undefined || value === null || value === "") return "—";
  return `${decimal(value, places)} ${currency}`;
}

/** A signed value, for P&L. The sign is explicit so zero reads as flat. */
export function signed(
  value: string | undefined | null,
  currency?: string,
  places = 2,
): string {
  if (value === undefined || value === null || value === "") return "—";
  const formatted = decimal(value, places);
  const prefix = value.startsWith("-") || formatted.startsWith("-") ? "" : "+";
  return currency ? `${prefix}${formatted} ${currency}` : `${prefix}${formatted}`;
}

/** "profit" | "loss" | "" for CSS, from a decimal string. */
export function tone(value: string | undefined | null): "profit" | "loss" | "" {
  if (!value) return "";
  const numeric = value.replace(/[^0-9.-]/g, "");
  if (numeric.startsWith("-") && Number.parseFloat(numeric) !== 0) return "loss";
  if (Number.parseFloat(numeric) > 0) return "profit";
  return "";
}

/** A fraction string ("0.0125") as a percentage ("1.25%"). */
export function percent(value: string | undefined | null, places = 2): string {
  if (value === undefined || value === null || value === "") return "—";
  const parsed = Number.parseFloat(value);
  if (!Number.isFinite(parsed)) return "—";
  // Percentages are presentational, so a float here cannot affect the ledger.
  return `${(parsed * 100).toFixed(places)}%`;
}

/** A ratio already expressed in percent ("12.5") as "12.5%". */
export function percentValue(value: string | number | undefined | null, places = 2): string {
  if (value === undefined || value === null || value === "") return "—";
  const parsed = typeof value === "number" ? value : Number.parseFloat(value);
  if (!Number.isFinite(parsed)) return "—";
  return `${parsed.toFixed(places)}%`;
}

const timeFormat = new Intl.DateTimeFormat("en-GB", {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

const dateTimeFormat = new Intl.DateTimeFormat("en-GB", {
  day: "2-digit",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});

const fullFormat = new Intl.DateTimeFormat("en-GB", {
  day: "2-digit",
  month: "short",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

export function time(iso: string | undefined | null): string {
  if (!iso) return "—";
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : timeFormat.format(date);
}

export function dateTime(iso: string | undefined | null): string {
  if (!iso) return "—";
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : dateTimeFormat.format(date);
}

export function fullDateTime(iso: string | undefined | null): string {
  if (!iso) return "—";
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : fullFormat.format(date);
}

/** "3m ago", "in 42m". Relative time is how a trader reads freshness. */
export function relative(iso: string | undefined | null): string {
  if (!iso) return "—";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";

  const seconds = Math.round((date.getTime() - Date.now()) / 1000);
  const future = seconds > 0;
  const abs = Math.abs(seconds);

  let text: string;
  if (abs < 10) text = "now";
  else if (abs < 60) text = `${abs}s`;
  else if (abs < 3600) text = `${Math.round(abs / 60)}m`;
  else if (abs < 86400) text = `${Math.round(abs / 3600)}h`;
  else text = `${Math.round(abs / 86400)}d`;

  if (text === "now") return "just now";
  return future ? `in ${text}` : `${text} ago`;
}

/** Seconds as a compact age, for quote freshness. */
export function age(seconds: number | undefined): string {
  if (seconds === undefined || !Number.isFinite(seconds)) return "—";
  if (seconds < 1) return "<1s";
  if (seconds < 60) return `${seconds.toFixed(0)}s`;
  if (seconds < 3600) return `${(seconds / 60).toFixed(0)}m`;
  return `${(seconds / 3600).toFixed(1)}h`;
}

/** Turn a snake_case code into readable words. */
export function humanise(value: string | undefined | null): string {
  if (!value) return "—";
  return value.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

/** Map a status or state to a status-dot tone. */
export function statusTone(value: string | undefined | null): "ok" | "warn" | "bad" | "info" {
  switch ((value ?? "").toLowerCase()) {
    case "ok":
    case "up":
    case "filled":
    case "clean":
    case "open":
    case "connected":
    case "ready":
    case "succeeded":
    case "alive":
      return "ok";
    case "degraded":
    case "partially_filled":
    case "cancel_pending":
    case "warning":
    case "medium":
    case "submitted":
    case "accepted":
    case "no_signal":
    case "skipped":
      return "warn";
    case "stale":
    case "invalid":
    case "no_data":
    case "down":
    case "rejected":
    case "failed":
    case "critical":
    case "high":
    case "unavailable":
    case "discrepancies_found":
      return "bad";
    default:
      return "info";
  }
}

/** A short id for display, with the full value kept for the title attribute. */
export function shortId(id: string | undefined | null): string {
  if (!id) return "—";
  return id.length <= 10 ? id : `${id.slice(0, 8)}…`;
}
