"use client";

/**
 * Shared presentational primitives.
 *
 * Kept small and unstyled-by-prop: components take content and a tone, and the
 * design system in globals.css decides how they look. A component that accepts
 * arbitrary styling would let each page drift, which is exactly how a dense
 * interface stops being readable.
 */

import type { ReactNode } from "react";

import { statusTone } from "@/lib/format";

export function Panel({
  title,
  note,
  actions,
  children,
  flush = false,
}: {
  title: string;
  note?: string;
  actions?: ReactNode;
  children: ReactNode;
  flush?: boolean;
}) {
  return (
    <section className="panel">
      <header className="panel-head">
        <h2 className="panel-title">{title}</h2>
        <div className="row-gap">
          {note ? <span className="panel-note">{note}</span> : null}
          {actions}
        </div>
      </header>
      <div className={flush ? "panel-body flush" : "panel-body"}>{children}</div>
    </section>
  );
}

export function Status({
  value,
  label,
  tone,
}: {
  value?: string | null;
  label?: string;
  tone?: "ok" | "warn" | "bad" | "info";
}) {
  return (
    <span className="status" data-tone={tone ?? statusTone(value)}>
      {label ?? value ?? "unknown"}
    </span>
  );
}

export function Badge({
  children,
  tone,
  title,
}: {
  children: ReactNode;
  tone?: "ok" | "bad" | "warn" | "accent";
  title?: string;
}) {
  return (
    <span className="badge" data-tone={tone} title={title}>
      {children}
    </span>
  );
}

export function Notice({
  tone = "info",
  children,
}: {
  tone?: "info" | "ok" | "warn" | "bad";
  children: ReactNode;
}) {
  return (
    <div className="notice" data-tone={tone === "info" ? undefined : tone}>
      {children}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}

export function Stat({
  label,
  value,
  sub,
  tone,
  delta,
  deltaTone,
}: {
  label: string;
  value: ReactNode;
  sub?: ReactNode;
  tone?: "profit" | "loss" | "";
  /**
   * A change against a stated comparison -- "+2.4%", "-18 bps", "3 fewer".
   *
   * Pre-formatted by the caller and never computed here: a component that did
   * its own arithmetic on money would have to parse a decimal string into a
   * float to do it, and this terminal does not do that. `lib/format.ts` is the
   * only place a figure is turned into text.
   *
   * `sub` should then say what the comparison IS. A delta with no stated
   * baseline is a number the reader cannot check.
   */
  delta?: string;
  /**
   * Whether the delta is good or bad -- which is NOT the same as whether it is
   * positive. A rising spread, a rising rejection rate and a rising drawdown
   * are all increases and all bad, and colouring them green because they carry
   * a plus sign would be actively misleading.
   *
   * Omit it entirely for a change that is neither: a count that moved, a
   * version that incremented. The badge then renders in muted text, which is
   * the honest answer to "is this good?" when the answer is "it depends".
   */
  deltaTone?: "profit" | "loss";
}) {
  return (
    <div className="stat">
      <div className="stat-label">{label}</div>
      <div className="stat-line">
        <div className={`stat-value ${tone ?? ""}`}>{value}</div>
        {delta ? (
          <span className="stat-delta" data-tone={deltaTone ?? "neutral"}>
            {delta}
          </span>
        ) : null}
      </div>
      {sub ? <div className="stat-sub">{sub}</div> : null}
    </div>
  );
}

export function StatStrip({ children }: { children: ReactNode }) {
  return <div className="stat-strip">{children}</div>;
}

/**
 * A utilisation meter.
 *
 * Turns amber past 75% and red past 90%. An operator scanning the risk page
 * should see which limit is close to binding without reading any numbers.
 */
export function Meter({ fraction }: { fraction: number }) {
  const clamped = Math.max(0, Math.min(1, Number.isFinite(fraction) ? fraction : 0));
  const tone = clamped >= 0.9 ? "bad" : clamped >= 0.75 ? "warn" : undefined;
  return (
    <div className="meter" role="img" aria-label={`${(clamped * 100).toFixed(0)}% of limit used`}>
      <div className="meter-fill" data-tone={tone} style={{ width: `${clamped * 100}%` }} />
    </div>
  );
}

/**
 * An inline sparkline drawn as SVG.
 *
 * Hand-drawn rather than pulled from a charting library: it is twenty lines of
 * path maths, and a dependency for this would be larger than the feature.
 */
export function Sparkline({
  values,
  height = 34,
  tone,
}: {
  values: number[];
  height?: number;
  tone?: "profit" | "loss";
}) {
  if (values.length < 2) {
    return <div className="empty tiny">not enough history to plot</div>;
  }

  const width = 100;
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;

  const points = values.map((value, index) => {
    const x = (index / (values.length - 1)) * width;
    const y = height - ((value - min) / span) * (height - 4) - 2;
    return `${x.toFixed(2)},${y.toFixed(2)}`;
  });

  const last = values[values.length - 1] ?? 0;
  const first = values[0] ?? 0;
  const colour =
    tone === "profit" || (tone === undefined && last >= first)
      ? "var(--profit)"
      : "var(--loss)";

  return (
    <svg
      className="sparkline"
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      role="img"
      aria-label="trend"
    >
      <polyline
        points={points.join(" ")}
        fill="none"
        stroke={colour}
        strokeWidth="1.2"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}

/** A right-aligned monospace numeric cell. */
export function Num({
  children,
  tone,
  title,
}: {
  children: ReactNode;
  tone?: "profit" | "loss" | "";
  title?: string;
}) {
  return (
    <td className={`mono right ${tone ?? ""}`} title={title}>
      {children}
    </td>
  );
}

export function Loading({ what }: { what: string }) {
  return <div className="empty">Loading {what}…</div>;
}

export function ErrorNote({ message }: { message: string }) {
  return <Notice tone="bad">{message}</Notice>;
}

export function PageHead({
  title,
  subtitle,
  actions,
}: {
  title: string;
  subtitle?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="page-head">
      <div>
        <h1 className="page-title">{title}</h1>
        {subtitle ? <p className="page-sub">{subtitle}</p> : null}
      </div>
      {actions ? <div className="row-gap">{actions}</div> : null}
    </div>
  );
}
