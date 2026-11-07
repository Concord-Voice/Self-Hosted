import { useCallback, useEffect, useRef, useState } from "react";

import { ApiContractError, ApiError } from "./api";
import {
  APPLIANCES,
  STEP_UP_ACTIONS,
  ServerRequestError,
  UPDATE_TAG,
  serverWriteApi,
  type Appliance,
  type ServerBackup,
  type ServerJob,
  type WriteAction,
} from "./server";
import { decodeRequestOptions, encodeAssertion } from "./webauthn";

// The Server routes share 30 requests a minute; the runbook says poll a job
// no faster than every 5 seconds.
const POLL_MS = 5000;

const ACTION_LABELS: Record<WriteAction, string> = {
  start: "Start",
  stop: "Stop",
  restart: "Restart",
  backup: "Back up",
  update: "Update",
  restore: "Restore",
};

// What each refusal means to the operator. Unknown codes fall back to the
// generic line.
const REFUSALS: Record<string, string> = {
  write_in_progress: "Another action is still running. Wait for it to finish.",
  stepup_required: "This action needs your security key.",
  stepup_failed: "The security key check failed. Try again.",
  stepup_unavailable:
    "Your account has no security key this server can check, so this action is not available.",
  audit_unavailable:
    "The audit log is not writable right now, so the action was refused.",
  agent_unavailable:
    "The host agent is not reachable. Install and start it, then try again.",
  agent_busy: "The host agent is busy. Try again shortly.",
  security_log_boundary_missing:
    "The security log folder is missing on the host. Restart refused before stopping anything.",
  untrusted_backups:
    "The backups folder is not owned by root, so the agent will not read it.",
  unknown_archive: "That backup is no longer in the backups folder.",
  invalid_tag: "Enter a release tag such as v0.3.1.",
  invalid_archive: "Choose a backup from the list.",
};

interface ServerActionsProps {
  // Ends the session on 401, or shows the access screen on a 403 that is
  // not a step-up refusal; returns null then.
  onSessionError: (error: unknown) => string | null;
  pollMs?: number;
}

function describe(
  error: unknown,
  onSessionError: ServerActionsProps["onSessionError"],
): string | null {
  if (error instanceof DOMException && error.name === "NotAllowedError") {
    return "Security key verification was canceled.";
  }
  if (
    error instanceof ServerRequestError &&
    error.code &&
    REFUSALS[error.code]
  ) {
    return REFUSALS[error.code];
  }
  if (error instanceof ApiContractError) {
    return "The server's response was not recognized. This console may be out of date.";
  }
  if (error instanceof ApiError) return onSessionError(error);
  return "The security key could not be used. Start again.";
}

const formatTime = (iso: string) => new Date(iso).toLocaleString();
const formatSize = (bytes: number) =>
  bytes >= 1 << 30
    ? `${(bytes / (1 << 30)).toFixed(1)} GiB`
    : `${(bytes / (1 << 20)).toFixed(1)} MiB`;

export function ServerActions({
  onSessionError,
  pollMs = POLL_MS,
}: Readonly<ServerActionsProps>) {
  const [jobs, setJobs] = useState<ServerJob[]>([]);
  const [backups, setBackups] = useState<ServerBackup[]>([]);
  const [appliance, setAppliance] = useState<Appliance | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);
  const [current, setCurrent] = useState<ServerJob | null>(null);
  const [tag, setTag] = useState("");
  const [archive, setArchive] = useState("");
  const pollRef = useRef<number | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const refresh = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    try {
      const [nextJobs, nextBackups] = await Promise.all([
        serverWriteApi.jobs(controller.signal),
        serverWriteApi.backups(controller.signal),
      ]);
      if (controller.signal.aborted) return;
      setJobs(nextJobs);
      setBackups(nextBackups);
      setArchive((chosen) =>
        nextBackups.some((b) => b.name === chosen)
          ? chosen
          : (nextBackups[0]?.name ?? ""),
      );
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof ServerRequestError && cause.appliance) {
        setAppliance(cause.appliance);
        return;
      }
      setError(describe(cause, onSessionError));
    }
  }, [onSessionError]);

  // Follows one job until it ends, then refreshes the lists.
  const follow = useCallback(
    (id: string) => {
      const tick = async () => {
        pollRef.current = null;
        try {
          const job = await serverWriteApi.job(id);
          setCurrent(job);
          if (job.state === "running") {
            pollRef.current = globalThis.setTimeout(() => void tick(), pollMs);
            return;
          }
          setStatus(
            job.state === "succeeded"
              ? ""
              : "The action failed. Its output is below.",
          );
          void refresh();
        } catch (cause) {
          setError(describe(cause, onSessionError));
        }
      };
      void tick();
    },
    [onSessionError, pollMs, refresh],
  );

  useEffect(() => {
    void refresh();
    return () => {
      abortRef.current?.abort();
      if (pollRef.current !== null) globalThis.clearTimeout(pollRef.current);
    };
  }, [refresh]);

  // A job started elsewhere (another tab or admin) is followed too.
  const running = jobs.find((job) => job.state === "running");
  useEffect(() => {
    if (running && pollRef.current === null && current?.id !== running.id) {
      follow(running.id);
    }
  }, [running, current, follow]);

  const run = async (
    action: WriteAction,
    args: Record<string, string> = {},
  ) => {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      const body: Record<string, unknown> = { ...args };
      if (STEP_UP_ACTIONS.has(action)) {
        setStatus("Touch your security key to confirm");
        const challenge = await serverWriteApi.stepUp(action);
        const credential = await navigator.credentials.get({
          publicKey: decodeRequestOptions(challenge.publicKey),
        });
        if (!credential)
          throw new DOMException("no credential", "NotAllowedError");
        body.stepup = {
          handle: challenge.handle,
          assertion: encodeAssertion(credential as PublicKeyCredential),
        };
      }
      setStatus(`${ACTION_LABELS[action]} started`);
      const job = await serverWriteApi.run(action, body);
      setCurrent(job);
      follow(job.id);
    } catch (cause) {
      setStatus("");
      setError(describe(cause, onSessionError));
    } finally {
      setBusy(false);
    }
  };

  if (appliance) {
    return (
      <section
        aria-labelledby="server-actions-title"
        className="workspace-section"
      >
        <div className="section-heading">
          <div>
            <h2 id="server-actions-title">Actions</h2>
          </div>
        </div>
        <output className="resource-notice">
          {APPLIANCES[appliance]} runs this server&apos;s containers. Start,
          stop, update and back it up from the {APPLIANCES[appliance]} Apps
          screen. This console only reads.
        </output>
      </section>
    );
  }

  const locked = busy || running !== undefined;
  const validTag = UPDATE_TAG.test(tag);

  return (
    <section
      aria-labelledby="server-actions-title"
      className="workspace-section"
    >
      <div className="section-heading">
        <div>
          <h2 id="server-actions-title">Actions</h2>
          <p>
            One action runs at a time. Stop, restart, update and restore ask for
            your security key. Every attempt is audited.
          </p>
        </div>
      </div>
      <div className="server-action-row">
        {(["start", "stop", "restart", "backup"] as const).map((action) => (
          <button
            className="server-action"
            disabled={locked}
            key={action}
            type="button"
            onClick={() => void run(action)}
          >
            {action === "backup" ? "Back up now" : ACTION_LABELS[action]}
          </button>
        ))}
      </div>
      <form
        className="series-controls server-log-controls"
        onSubmit={(event) => {
          event.preventDefault();
          if (validTag) void run("update", { tag });
        }}
      >
        <label>
          <span>Update to release tag</span>
          <input
            aria-describedby="server-update-help"
            placeholder="v0.3.1"
            value={tag}
            onChange={(event) => setTag(event.currentTarget.value.trim())}
          />
        </label>
        <button
          className="server-action"
          disabled={locked || !validTag}
          type="submit"
        >
          Update
        </button>
      </form>
      <p className="server-help" id="server-update-help">
        The host installs only a tag signed by a Concord release key.
      </p>
      <form
        className="series-controls server-log-controls"
        onSubmit={(event) => {
          event.preventDefault();
          if (archive) void run("restore", { archive });
        }}
      >
        <label>
          <span>Restore from backup</span>
          <select
            disabled={backups.length === 0}
            value={archive}
            onChange={(event) => setArchive(event.currentTarget.value)}
          >
            {backups.length === 0 && <option value="">No backups yet</option>}
            {backups.map((backup) => (
              <option key={backup.name} value={backup.name}>
                {formatTime(backup.modified)} ({formatSize(backup.size)})
              </option>
            ))}
          </select>
        </label>
        <button
          className="server-action"
          disabled={locked || !archive}
          type="submit"
        >
          Restore
        </button>
      </form>
      {status && (
        <output className="resource-notice resource-loading">{status}</output>
      )}
      {error && <output className="resource-notice">{error}</output>}
      {current && (
        <>
          <p className="server-logs-caption">
            {ACTION_LABELS[current.action]}: {current.state}
            {current.archive ? `, wrote ${current.archive}` : ""}
          </p>
          <textarea
            aria-label={`${ACTION_LABELS[current.action]} output`}
            className="server-logs"
            readOnly
            rows={12}
            value={current.output ?? ""}
          />
        </>
      )}
      {jobs.length > 0 && (
        <div className="table-scroll server-actions-jobs">
          <table>
            <thead>
              <tr>
                <th scope="col">Action</th>
                <th scope="col">State</th>
                <th scope="col">Started</th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((job) => (
                <tr key={job.id}>
                  <th scope="row">{ACTION_LABELS[job.action]}</th>
                  <td>{job.state}</td>
                  <td>{formatTime(job.started)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
