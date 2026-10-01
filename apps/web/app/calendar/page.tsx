"use client";

/**
 * Economic calendar and news.
 *
 * Both feeds come from provider abstractions with mock implementations in this
 * build. That is stated on the page rather than implied, because a calendar
 * that looks authoritative and is not is worse than no calendar: an operator
 * would plan around it.
 */

import { useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { api } from "@/lib/api";
import { dateTime, fullDateTime, relative, time } from "@/lib/format";
import { useAsync, useNow, useVantage } from "@/lib/store";

const RANGES = [
  { key: "today", label: "Today" },
  { key: "week", label: "This week" },
  { key: "upcoming", label: "Upcoming" },
];

export default function CalendarPage() {
  const { account } = useVantage();
  const accountId = account?.id;
  const [range, setRange] = useState("week");
  const [impact, setImpact] = useState<string>("");

  const calendar = useAsync(() => api.calendar(range, impact || undefined), [range, impact]);
  const news = useAsync(() => api.news(), []);
  const risk = useAsync(
    () => (accountId ? api.riskLimits(accountId) : Promise.resolve(null)),
    [accountId],
  );

  const events = calendar.data?.events ?? [];
  const headlines = news.data?.news ?? [];
  const limits = risk.data?.limits;

  // Ticking, so a blackout badge appears and clears on its own rather than
  // only when the page is reloaded.
  const now = useNow(15_000);
  const blackoutBefore = (limits?.event_blackout_before_minutes ?? 0) * 60_000;
  const blackoutAfter = (limits?.event_blackout_after_minutes ?? 0) * 60_000;

  const inBlackout = (event: { ScheduledAt: string; Impact: string }) => {
    if (!limits?.block_on_high_impact_events || event.Impact !== "high") return false;
    const at = new Date(event.ScheduledAt).getTime();
    if (!Number.isFinite(at)) return false;
    return now >= at - blackoutBefore && now <= at + blackoutAfter;
  };

  const activeBlackouts = events.filter(inBlackout);

  return (
    <>
      <Notice>
        {calendar.data?.note ??
          "Calendar and news are served through provider interfaces. This build uses deterministic mock providers; no third-party site is scraped and no provider terms are bypassed."}
      </Notice>

      {activeBlackouts.length > 0 ? (
        <Notice tone="bad">
          <strong>Event blackout in force.</strong> Automated orders are refused around{" "}
          {activeBlackouts.map((event) => event.EventName).join(", ")}. A manual order still
          goes through: you have been told, and blocking a person from closing a position
          before a release would be worse than the risk it prevents.
        </Notice>
      ) : null}

      <div className="grid sidebar-right">
        <div>
          <Panel
            title="Economic calendar"
            note={`${events.length} event(s)`}
            actions={
              <>
                <div className="segmented">
                  {RANGES.map((option) => (
                    <button
                      key={option.key}
                      data-selected={option.key === range}
                      onClick={() => setRange(option.key)}
                    >
                      {option.label}
                    </button>
                  ))}
                </div>
                <div className="segmented">
                  {["", "high", "medium"].map((option) => (
                    <button
                      key={option || "all"}
                      data-selected={option === impact}
                      onClick={() => setImpact(option)}
                    >
                      {option === "" ? "All" : option}
                    </button>
                  ))}
                </div>
              </>
            }
            flush
          >
            {calendar.loading ? (
              <Empty>Loading the calendar…</Empty>
            ) : events.length === 0 ? (
              <Empty>No events in this window.</Empty>
            ) : (
              <div className="table-scroll tall">
                <table>
                  <thead>
                    <tr>
                      <th>When</th>
                      <th className="right">In</th>
                      <th>Impact</th>
                      <th>Currency</th>
                      <th>Event</th>
                      <th className="right">Actual</th>
                      <th className="right">Forecast</th>
                      <th className="right">Previous</th>
                      <th>Blackout</th>
                    </tr>
                  </thead>
                  <tbody>
                    {events.map((event) => {
                      const blackout = inBlackout(event);
                      return (
                        <tr key={event.ID}>
                          <td className="mono" title={fullDateTime(event.ScheduledAt)}>
                            {dateTime(event.ScheduledAt)}
                          </td>
                          <td className="mono right muted">{relative(event.ScheduledAt)}</td>
                          <td>
                            <Status
                              value={event.Impact}
                              tone={
                                event.Impact === "high"
                                  ? "bad"
                                  : event.Impact === "medium"
                                    ? "warn"
                                    : "ok"
                              }
                            />
                          </td>
                          <td className="mono">{event.Currency}</td>
                          <td>{event.EventName}</td>
                          <td className="mono right">{event.Actual ?? "-"}</td>
                          <td className="mono right muted">{event.Forecast ?? "-"}</td>
                          <td className="mono right muted">{event.Previous ?? "-"}</td>
                          <td>
                            {blackout ? (
                              <span className="badge" data-tone="bad">
                                blocking
                              </span>
                            ) : (
                              <span className="tiny muted"> · </span>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>

          <Panel
            title="News"
            note={headlines.length > 0 ? `${headlines.length} item(s)` : undefined}
            flush
          >
            {news.loading ? (
              <Empty>Loading news…</Empty>
            ) : headlines.length === 0 ? (
              <Empty>No news items.</Empty>
            ) : (
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th className="right">Published</th>
                      <th>Source</th>
                      <th>Headline</th>
                      <th>Instruments</th>
                      <th>Sentiment</th>
                    </tr>
                  </thead>
                  <tbody>
                    {headlines.map((item) => (
                      <tr key={item.ID}>
                        <td
                          className="mono right muted"
                          title={fullDateTime(item.PublishedAt)}
                        >
                          {time(item.PublishedAt)}
                        </td>
                        <td className="muted">{item.Source}</td>
                        <td className="wrap" title={item.Summary ?? undefined}>
                          {item.Headline}
                        </td>
                        <td className="mono tiny muted">
                          {(item.RelatedInstruments ?? []).join(", ") ||
                            (item.RelatedCurrencies ?? []).join(", ") ||
                            "-"}
                        </td>
                        <td className="mono tiny muted">{item.Sentiment ?? "-"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>
        </div>

        <div>
          <Panel title="Blackout policy" flush>
            <div className="panel-body">
              {limits ? (
                <dl className="kv">
                  <dt>Before a high-impact event</dt>
                  <dd>{limits.event_blackout_before_minutes} minutes</dd>
                  <dt>After</dt>
                  <dd>{limits.event_blackout_after_minutes} minutes</dd>
                  <dt>Enforced</dt>
                  <dd className={limits.block_on_high_impact_events ? "profit" : "loss"}>
                    {limits.block_on_high_impact_events ? "yes" : "no"}
                  </dd>
                </dl>
              ) : (
                <p className="small muted" style={{ marginTop: 0 }}>
                  Blackout windows are configured per account, on the Risk page.
                </p>
              )}
              <p className="small muted" style={{ marginBottom: 0 }}>
                Only high-impact events halt trading. Blacking out on every scheduled release
                would leave almost no tradable window on a gold instrument, which is a
                different way of not having a risk control.
              </p>
            </div>
          </Panel>

          <Panel title="Why events matter here" flush>
            <div className="panel-body">
              <p className="small muted" style={{ marginTop: 0 }}>
                Around a high-impact release the spread widens and slippage stops resembling
                anything a backtest measured. A stop placed before the release is not the stop
                that gets filled. Refusing to open exposure into that window costs the
                occasional good trade and removes a class of loss no model here can price.
              </p>
              <p className="small muted" style={{ marginBottom: 0 }}>
                The blackout applies to <strong>automated</strong> orders. A manual order is
                allowed through with the event shown, because refusing to let a person out
                during a release would be a risk control that creates risk, and an operator
                who can see the event can decide for themselves.
              </p>
            </div>
          </Panel>

          <Panel title="Providers" flush>
            <div className="panel-body">
              <dl className="kv">
                <dt>Calendar</dt>
                <dd>mock, deterministic</dd>
                <dt>News</dt>
                <dd>mock, deterministic</dd>
                <dt>Scraping</dt>
                <dd>none</dd>
              </dl>
              <p className="tiny muted" style={{ marginBottom: 0 }}>
                Both sit behind the CalendarProvider and NewsProvider interfaces, and the
                names above are what those implementations report about themselves rather
                than a label this page asserts. A licensed feed is one adapter plus
                configuration; nothing above the interfaces knows which is in use.
              </p>
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}
