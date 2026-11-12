import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError } from "./api";
import { ServerActions } from "./ServerActions";
import {
  LOG_LINE_CHOICES,
  LOG_SERVICES,
  serverApi,
  type LogLineChoice,
  type LogService,
  type ServerHealth,
  type ServerService,
  type ServerStatus,
} from "./server";
import { Status, type DisplayStatus } from "./workspaces";

interface LoadedLogs {
  service: LogService;
  lines: LogLineChoice;
  text: string;
}

interface ServerWorkspaceProps {
  onForbidden: () => void;
  onUnauthorized: () => void;
}

function errorCopy(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 503) {
      return "The host agent is not reachable. Install and start it, then run concord-selfhost up.";
    }
    if (error.status === 429)
      return "The host agent is busy. Try again shortly.";
  }
  return "The host agent could not answer this request.";
}

function serviceStatus({ state, health }: ServerService): DisplayStatus {
  if (state === "restarting") return "degraded";
  if (state === "exited" || state === "dead") return "stopped";
  if (state !== "running") return "unknown";
  if (health === "unhealthy") return "degraded";
  if (health === "healthy" || health === "none") return "healthy";
  return "unknown";
}

export function ServerWorkspace({
  onForbidden,
  onUnauthorized,
}: Readonly<ServerWorkspaceProps>) {
  const [status, setStatus] = useState<ServerStatus | null>(null);
  const [health, setHealth] = useState<ServerHealth | null>(null);
  const [statusError, setStatusError] = useState<string | null>(null);
  const [healthError, setHealthError] = useState<string | null>(null);
  const [loading, setLoading] = useState(0);
  const [service, setService] = useState<LogService>("control-plane");
  const [lines, setLines] = useState<LogLineChoice>(200);
  const [logs, setLogs] = useState<LoadedLogs | null>(null);
  const [logsError, setLogsError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // One controller per view: a refresh or unmount aborts what is in flight,
  // so a slow, older answer can never overwrite a newer one.
  const overviewAbortRef = useRef<AbortController | null>(null);
  const logsAbortRef = useRef<AbortController | null>(null);

  // Returns the copy to show, or null when the session itself ended.
  const handle = useCallback(
    (error: unknown): string | null => {
      if (error instanceof ApiError && error.status === 401) {
        onUnauthorized();
        return null;
      }
      if (error instanceof ApiError && error.status === 403) {
        onForbidden();
        return null;
      }
      return errorCopy(error);
    },
    [onForbidden, onUnauthorized],
  );

  const loadOverview = useCallback(() => {
    overviewAbortRef.current?.abort();
    const controller = new AbortController();
    overviewAbortRef.current = controller;
    const { signal } = controller;
    // Status answers in moments; health can take a minute. Load them apart so
    // the table does not wait on the checks, and clear old data on failure so
    // a stale reading never sits beside a fresh error.
    const load = async <T,>(
      fetchView: (signal: AbortSignal) => Promise<T>,
      setData: (value: T | null) => void,
      setError: (value: string | null) => void,
    ) => {
      setError(null);
      setLoading((count) => count + 1);
      try {
        const value = await fetchView(signal);
        if (!signal.aborted) setData(value);
      } catch (error) {
        if (signal.aborted) return;
        setData(null);
        setError(handle(error));
      } finally {
        setLoading((count) => count - 1);
      }
    };
    void load(serverApi.status, setStatus, setStatusError);
    void load(serverApi.health, setHealth, setHealthError);
  }, [handle]);

  useEffect(() => {
    loadOverview();
    return () => {
      overviewAbortRef.current?.abort();
      logsAbortRef.current?.abort();
    };
  }, [loadOverview]);

  const loadLogs = async () => {
    logsAbortRef.current?.abort();
    const controller = new AbortController();
    logsAbortRef.current = controller;
    const requested = { service, lines };
    setBusy(true);
    setLogsError(null);
    try {
      const text = await serverApi.logs(service, lines, controller.signal);
      if (!controller.signal.aborted) setLogs({ ...requested, text });
    } catch (error) {
      if (!controller.signal.aborted) setLogsError(handle(error));
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  };

  return (
    <div className="workspace-content">
      <section
        aria-labelledby="server-services-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="server-services-title">Containers</h2>
            <p>
              Read from this server by the host agent. Each view is audited.
            </p>
          </div>
          <button
            className="server-action"
            disabled={loading > 0}
            type="button"
            onClick={loadOverview}
          >
            {loading > 0 ? "Refreshing" : "Refresh"}
          </button>
        </div>
        {loading > 0 && !status && !statusError && (
          <output className="resource-notice resource-loading">
            Loading containers
          </output>
        )}
        {statusError && (
          <output className="resource-notice">{statusError}</output>
        )}
        {status && (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th scope="col">Service</th>
                  <th scope="col">Status</th>
                  <th scope="col">State</th>
                  <th scope="col">Image</th>
                </tr>
              </thead>
              <tbody>
                {status.services.map((item) => (
                  <tr key={item.name}>
                    <th scope="row">{item.service}</th>
                    <td>
                      <Status state={serviceStatus(item)} />
                    </td>
                    <td>
                      {item.state}
                      {item.exit_code === null
                        ? ""
                        : ` (exit ${item.exit_code})`}
                    </td>
                    <td className="server-image">{item.image}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <ServerActions onSessionError={handle} />

      {healthError && (
        <output className="resource-notice">
          Health checks: {healthError}
        </output>
      )}
      {health && (
        <section
          aria-labelledby="server-health-title"
          className="workspace-section"
        >
          <div className="section-heading">
            <div>
              <h2 id="server-health-title">Health checks</h2>
              <p>
                {health.ok ? "All checks passed" : "At least one check failed"}
              </p>
            </div>
          </div>
          <ul className="server-checks">
            {health.checks.map((check) => (
              <li key={check.name}>
                <Status state={check.ok ? "healthy" : "degraded"} />
                <span>{check.name}</span>
              </li>
            ))}
          </ul>
        </section>
      )}

      <section
        aria-labelledby="server-logs-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="server-logs-title">Logs</h2>
            <p>The most recent lines from one service</p>
          </div>
        </div>
        <div className="series-controls server-log-controls">
          <label>
            <span>Service</span>
            <select
              value={service}
              onChange={(event) => {
                const next = event.currentTarget.value as LogService;
                if (LOG_SERVICES.includes(next)) setService(next);
              }}
            >
              {LOG_SERVICES.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
          </label>
          <label>
            <span>Lines</span>
            <select
              value={lines}
              onChange={(event) => {
                const next = Number(event.currentTarget.value) as LogLineChoice;
                if (LOG_LINE_CHOICES.includes(next)) setLines(next);
              }}
            >
              {LOG_LINE_CHOICES.map((count) => (
                <option key={count} value={count}>
                  {count}
                </option>
              ))}
            </select>
          </label>
          <button
            className="server-action"
            disabled={busy}
            type="button"
            onClick={() => void loadLogs()}
          >
            {busy ? "Loading" : "Show logs"}
          </button>
        </div>
        {logsError && <output className="resource-notice">{logsError}</output>}
        {logs !== null && (
          <>
            <p className="server-logs-caption">
              {logs.service}, last {logs.lines} lines
            </p>
            {/* A read-only text area: keyboard users can focus and scroll it in
                every browser, including WebKit, which does not focus scroll
                containers. */}
            <textarea
              aria-label={`${logs.service} logs`}
              className="server-logs"
              readOnly
              rows={20}
              value={logs.text === "" ? "No log lines." : logs.text}
            />
          </>
        )}
      </section>
    </div>
  );
}
