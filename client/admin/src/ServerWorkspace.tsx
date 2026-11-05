import { useCallback, useEffect, useState } from "react";

import { ApiError } from "./api";
import {
  LOG_LINE_CHOICES,
  LOG_SERVICES,
  serverApi,
  type LogService,
  type ServerHealth,
  type ServerService,
  type ServerStatus,
} from "./server";
import { Status, type DisplayStatus } from "./workspaces";

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
  if (state !== "running") return state === "unknown" ? "unknown" : "stopped";
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
  const [overviewError, setOverviewError] = useState<string | null>(null);
  const [service, setService] = useState<LogService>("control-plane");
  const [lines, setLines] = useState<number>(200);
  const [logs, setLogs] = useState<string | null>(null);
  const [logsError, setLogsError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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

  const loadOverview = useCallback(async () => {
    setOverviewError(null);
    try {
      const [nextStatus, nextHealth] = await Promise.all([
        serverApi.status(),
        serverApi.health(),
      ]);
      setStatus(nextStatus);
      setHealth(nextHealth);
    } catch (error) {
      setOverviewError(handle(error));
    }
  }, [handle]);

  useEffect(() => {
    void loadOverview();
  }, [loadOverview]);

  const loadLogs = async () => {
    setBusy(true);
    setLogsError(null);
    try {
      setLogs(await serverApi.logs(service, lines));
    } catch (error) {
      setLogsError(handle(error));
    } finally {
      setBusy(false);
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
            type="button"
            onClick={() => void loadOverview()}
          >
            Refresh
          </button>
        </div>
        {overviewError && (
          <output className="resource-notice">{overviewError}</output>
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
              onChange={(event) => setLines(Number(event.currentTarget.value))}
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
          <pre aria-label={`${service} logs`} className="server-logs">
            {logs === "" ? "No log lines." : logs}
          </pre>
        )}
      </section>
    </div>
  );
}
