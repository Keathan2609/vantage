"use client";

import { useState } from "react";

import { Empty, Notice, Panel, Status } from "@/components/ui";
import { ApiError, api } from "@/lib/api";
import { dateTime, humanise } from "@/lib/format";
import { useAsync } from "@/lib/store";

export default function MlPage() {
  const models = useAsync(() => api.models(), []);
  const instruments = useAsync(() => api.instruments(true), []);

  const [modelKey, setModelKey] = useState("xauusd_direction");
  const [algorithm, setAlgorithm] = useState("logistic_regression");
  const [instrumentId, setInstrumentId] = useState("XAUUSD.m");
  const [days, setDays] = useState(45);
  const [horizon, setHorizon] = useState(1);
  const [seed, setSeed] = useState(42);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Awaited<ReturnType<typeof api.trainModel>> | null>(
    null,
  );

  const rows = models.data?.models ?? [];

  const train = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      const to = new Date();
      const from = new Date(to.getTime() - days * 24 * 3600 * 1000);
      const response = await api.trainModel({
        model_key: modelKey,
        algorithm,
        instrument_id: instrumentId,
        timeframe: "1h",
        from: from.toISOString(),
        to: to.toISOString(),
        horizon,
        seed,
      });
      setResult(response);
      models.reload();
    } catch (cause) {
      setError(
        cause instanceof ApiError ? `${cause.code}: ${cause.message}` : "Training failed",
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Notice>
        Models are trained on chronological windows with an embargo between them, and every
        score is reported against the majority-class baseline. A model that cannot beat
        always-guessing-the-common-direction is reported as not beating it. Models filter
        signals; they never initiate one, and they cannot promote themselves.
      </Notice>

      <div className="grid sidebar-right">
        <div>
          <Panel title="Trained model versions" note={`${rows.length} version(s)`} flush>
            {models.loading ? (
              <Empty>Loading…</Empty>
            ) : rows.length === 0 ? (
              <Empty>No models trained yet.</Empty>
            ) : (
              <div className="table-scroll tall">
                <table>
                  <thead>
                    <tr>
                      <th>Model</th>
                      <th>Task</th>
                      <th>Algorithm</th>
                      <th className="right">Ver</th>
                      <th>Lifecycle</th>
                      <th className="right">Seed</th>
                      <th>Train window</th>
                      <th>Test window</th>
                      <th className="right">Trained</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((model) => (
                      <tr key={model.ID}>
                        <td>{model.ModelKey}</td>
                        <td className="muted">{humanise(model.Task)}</td>
                        <td className="muted">{humanise(model.Algorithm)}</td>
                        <td className="mono right">{model.Version}</td>
                        <td>
                          <span className="badge">{model.Lifecycle}</span>
                        </td>
                        <td className="mono right muted">{model.RandomSeed}</td>
                        <td className="mono tiny muted">
                          {dateTime(model.TrainStart)} → {dateTime(model.TrainEnd)}
                        </td>
                        <td className="mono tiny muted">
                          {model.TestStart
                            ? `${dateTime(model.TestStart)} → ${dateTime(model.TestEnd)}`
                            : "—"}
                        </td>
                        <td className="mono right muted">{dateTime(model.CreatedAt)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>

          {rows.some((model) => (model.evaluations ?? []).length > 0) ? (
            <Panel title="Evaluations" note="train, validation and test, in that order" flush>
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th>Model</th>
                      <th>Split</th>
                      <th className="right">Rows</th>
                      <th className="right">Accuracy</th>
                      <th className="right">Baseline</th>
                      <th className="right">Over baseline</th>
                      <th>Beats baseline</th>
                      <th className="right">Brier</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.flatMap((model) =>
                      (model.evaluations ?? []).map((evaluation) => {
                        const metrics = evaluation.Metrics ?? {};
                        const beats = metrics.beats_baseline === true;
                        return (
                          <tr key={`${model.ID}-${evaluation.Split}`}>
                            <td>
                              {model.ModelKey} v{model.Version}
                            </td>
                            <td>{evaluation.Split}</td>
                            <td className="mono right muted">{String(metrics.rows ?? "—")}</td>
                            <td className="mono right">{String(metrics.accuracy ?? "—")}</td>
                            <td className="mono right muted">
                              {String(metrics.majority_class_baseline ?? "—")}
                            </td>
                            <td
                              className={`mono right ${
                                String(metrics.accuracy_over_baseline ?? "").startsWith("-")
                                  ? "loss"
                                  : "profit"
                              }`}
                            >
                              {String(metrics.accuracy_over_baseline ?? "—")}
                            </td>
                            <td>
                              <Status
                                value={beats ? "yes" : "no"}
                                tone={beats ? "ok" : "bad"}
                              />
                            </td>
                            <td className="mono right muted">
                              {String(metrics.brier_score ?? "—")}
                            </td>
                          </tr>
                        );
                      }),
                    )}
                  </tbody>
                </table>
              </div>
              <div className="panel-body">
                <p className="tiny muted" style={{ margin: 0 }}>
                  Accuracy alone is meaningless on imbalanced data: a market that rose on 53%
                  of bars hands 53% accuracy to a model that always says &ldquo;up&rdquo;. The
                  Brier score measures calibration, which matters more for a filter — a model
                  whose &ldquo;60%&rdquo; means 60% is usable; one whose &ldquo;90%&rdquo;
                  means 55% is dangerous.
                </p>
              </div>
            </Panel>
          ) : null}
        </div>

        <div>
          <Panel title="Train a model">
            {error ? <Notice tone="bad">{error}</Notice> : null}

            <div className="field">
              <label htmlFor="key">Model key</label>
              <input
                id="key"
                value={modelKey}
                onChange={(event) => setModelKey(event.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="algorithm">Algorithm</label>
              <select
                id="algorithm"
                value={algorithm}
                onChange={(event) => setAlgorithm(event.target.value)}
              >
                <option value="logistic_regression">Logistic regression</option>
                <option value="random_forest">Random forest</option>
                <option value="gradient_boosting">Gradient boosting</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="instrument">Instrument</label>
              <select
                id="instrument"
                value={instrumentId}
                onChange={(event) => setInstrumentId(event.target.value)}
              >
                {(instruments.data?.instruments ?? []).map((instrument) => (
                  <option key={instrument.id} value={instrument.id}>
                    {instrument.symbol}
                  </option>
                ))}
              </select>
            </div>
            <div className="field-row">
              <div className="field">
                <label htmlFor="days">History (days)</label>
                <select
                  id="days"
                  value={days}
                  onChange={(event) => setDays(Number(event.target.value))}
                >
                  <option value={30}>30</option>
                  <option value={45}>45</option>
                </select>
              </div>
              <div className="field">
                <label htmlFor="horizon">Label horizon (bars)</label>
                <select
                  id="horizon"
                  value={horizon}
                  onChange={(event) => setHorizon(Number(event.target.value))}
                >
                  <option value={1}>1</option>
                  <option value={3}>3</option>
                  <option value={5}>5</option>
                </select>
              </div>
            </div>
            <div className="field">
              <label htmlFor="seed">Random seed (recorded for reproducibility)</label>
              <input
                id="seed"
                value={seed}
                onChange={(event) => setSeed(Number(event.target.value) || 0)}
                inputMode="numeric"
              />
            </div>

            <button
              data-variant="primary"
              onClick={() => void train()}
              disabled={busy}
              style={{ width: "100%" }}
            >
              {busy ? "Training…" : "Train"}
            </button>
            <p className="tiny muted" style={{ marginTop: 8, marginBottom: 0 }}>
              Splits are 60% train, 20% validation, 20% test, chronologically, with an
              embargo of twice the label horizon between windows.
            </p>
          </Panel>

          {result ? (
            <Panel title="Training result" note={result.lifecycle} flush>
              <div className="panel-body">
                <dl className="kv">
                  <dt>Version</dt>
                  <dd>{result.version}</dd>
                  <dt>Algorithm</dt>
                  <dd>{humanise(result.algorithm)}</dd>
                  <dt>Lifecycle</dt>
                  <dd>{result.lifecycle}</dd>
                  <dt>Dataset hash</dt>
                  <dd className="tiny">{result.dataset_hash.slice(0, 20)}</dd>
                </dl>

                <div className="panel-title" style={{ margin: "12px 0 5px" }}>
                  Windows
                </div>
                <table>
                  <tbody>
                    {Object.entries(result.windows).map(([name, window]) => (
                      <tr key={name}>
                        <td className="muted">{name}</td>
                        <td className="mono right tiny">
                          {String(window.rows ?? 0)} rows
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>

                {result.warnings.map((warning, index) => (
                  <Notice key={index} tone="warn">
                    {warning}
                  </Notice>
                ))}

                <p className="tiny muted" style={{ marginBottom: 0 }}>
                  {result.note}
                </p>
              </div>
            </Panel>
          ) : null}

          <Panel title="Lifecycle" flush>
            <div className="panel-body">
              <p className="small muted" style={{ marginTop: 0 }}>
                Every model starts EXPERIMENTAL. PAPER is this build&rsquo;s ceiling, enforced
                by a database constraint as well as by the API. A model cannot modify risk
                limits, cannot bypass trading authority, and cannot initiate a trade.
              </p>
              <p className="small muted" style={{ marginBottom: 0 }}>
                When inference fails or a feature is unavailable, the consuming strategy
                returns NO TRADE rather than falling back to an unfiltered signal —
                otherwise &ldquo;the model is broken&rdquo; would silently become
                &ldquo;trade without the filter&rdquo;.
              </p>
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}
