import { useCallback, useEffect, useRef, useState } from "react";

import { ApiContractError, ApiError } from "./api";
import {
  APPLIANCES,
  ServerRequestError,
  UPDATE_TAG,
  serverWriteApi,
  type Appliance,
  type ServerActionArguments,
  type ServerBackup,
  type ServerJob,
  type WriteAction,
} from "./server";
import {
  KEY_NOT_USED,
  decodeRequestOptions,
  encodeAssertion,
  keyNotUsed,
} from "./webauthn";

// The Server routes share 30 requests a minute; the runbook says poll a job
// no faster than every 5 seconds. The jobs list is checked every 30 seconds
// so a job started in another tab or by another admin locks the buttons.
const POLL_MS = 5000;
const JOBS_EVERY = 6;

const ACTION_LABELS: Record<WriteAction, string> = {
  start: "Start",
  stop: "Stop",
  restart: "Restart",
  backup: "Back up",
  update: "Update",
  restore: "Restore",
};

// What each refusal means to the operator. Unknown codes fall back to the
// generic line, and only the table's own keys count, so a code such as
// "constructor" or "__proto__" is unknown too.
const REFUSALS: Record<string, string> = {
  write_in_progress: "Another action is still running. Wait for it to finish.",
  stepup_required: "This action needs your security key.",
  stepup_failed: "The security key check failed. Try again.",
  authorization_denied:
    "The host agent rejected this admin's security key proof. Touch a key a host-root operator has imported with concord-agent authorizer import; a key added since the last import must be exported and imported again. If this persists, ask a host-root operator to check the import.",
  authorizer_unavailable:
    "The host agent cannot load its trusted authorizers. Ask a host-root operator to check the authorizer file and import it with concord-agent authorizer import.",
  peer_forbidden:
    "The host agent rejected the control-plane connection. Check the service identity and agent socket permissions on the host.",
  authorization_busy:
    "Too many security key requests are still open on the host. A request you did not finish, such as a cancelled key prompt, expires after 2 minutes. Wait, then try again.",
  invalid_authorization:
    "The host agent did not accept this authorization request. Reload the console and start again.",
  backup_rate_limited:
    "Backups and updates are limited to one attempt per hour, because an update takes a backup first. Wait until the hour has passed before trying again.",
  update_downgrade_refused:
    "That release is older than the one this server runs. The host agent does not move a server back to an older release.",
  running_version_unknown:
    "The host agent cannot read which release this server runs, so it refused the update. Ask a host-root operator to check the VERSION file in the install folder.",
  stepup_unavailable:
    "The host agent could not start a security key check. Try again shortly. If this keeps happening, ask a host operator to check that the agent is running.",
  audit_unavailable:
    "The audit log is not writable right now, so the action was refused.",
  agent_unavailable:
    "The host agent did not answer. Check that it is installed and running. An action you just started may still run, so check the jobs list before you start it again.",
  job_unavailable:
    "The host agent could not start the action, so nothing ran. Try again shortly. If this keeps happening, ask a host operator to check the agent log.",
  alerts_not_configured:
    "Host actions are off because this host has no out-of-band alert set up. Ask a host operator to configure the agent alert as the host agent runbook describes.",
  unknown_action:
    "The host agent does not know this action. Ask a host operator to update the agent to this server's release.",
  unexpected_argument:
    "The host agent refused an argument this action does not take. Reload the console and start again.",
  invalid_body:
    "The host agent could not read the request. Reload the console and start again.",
  // The control plane's own codes, for failures around the agent call.
  agent_error:
    "The host agent sent an answer the server could not use. An action you just started may still run, so check the jobs list, then try again. If this keeps happening, ask a host operator to check the host agent's journal.",
  internal_error:
    "The server failed before it reached the host agent, so nothing ran. Try again. If this keeps happening, ask a host operator to check the control plane log.",
  invalid_request:
    "The server refused this request because its action or arguments were not valid. Reload the console and try again. If this keeps happening, ask a host operator to check the control plane log.",
  agent_busy: "The host agent is busy. Try again shortly.",
  security_log_boundary_missing:
    "The security log folder is missing on the host. Restart refused before stopping anything.",
  untrusted_backups:
    "The backups folder is not owned by root, so the agent will not read it.",
  unknown_archive: "That backup is no longer in the backups folder.",
  unknown_job: "The host agent no longer knows that action.",
  appliance_managed:
    "An appliance runs this server's containers, so this console only reads.",
  rate_limited: "Too many requests. Wait a minute, then try again.",
  invalid_tag: "Enter a release tag such as v0.3.1.",
  invalid_archive: "Choose a backup from the list.",
  writes_disabled:
    "Host writes are switched off because the agent has no audit log or no trusted authorizer. Ask a host-root operator to check the agent log and import this admin's authorizer.",
  stepup_binding_mismatch:
    "The request changed after your security key was requested. Start the action again.",
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
  if (keyNotUsed(error)) return KEY_NOT_USED;
  if (
    error instanceof ServerRequestError &&
    error.code !== null &&
    Object.hasOwn(REFUSALS, error.code)
  ) {
    return REFUSALS[error.code];
  }
  if (error instanceof ApiContractError) {
    return "The server's response was not recognized. This console may be out of date.";
  }
  if (error instanceof ApiError) return onSessionError(error);
  return "The security key could not be used. Start again.";
}

// Worth asking again: rate limits, a busy or restarting agent, a dropped
// connection.
const transient = (error: unknown) =>
  error instanceof ApiError &&
  (error.status === 0 || error.status === 429 || error.status === 503);

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
  const followRef = useRef<AbortController | null>(null);

  const refresh = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    // Each list stands alone: an untrusted backups folder must not hide a
    // running job.
    const [nextJobs, nextBackups] = await Promise.allSettled([
      serverWriteApi.jobs(controller.signal),
      serverWriteApi.backups(controller.signal),
    ]);
    if (controller.signal.aborted) return;
    const failures = [nextJobs, nextBackups].flatMap((result) =>
      result.status === "rejected" ? [result.reason as unknown] : [],
    );
    const managed = failures.find(
      (cause) => cause instanceof ServerRequestError && cause.appliance,
    ) as ServerRequestError | undefined;
    if (managed?.appliance) {
      setAppliance(managed.appliance);
      return;
    }
    if (nextJobs.status === "fulfilled") setJobs(nextJobs.value);
    if (nextBackups.status === "fulfilled") {
      const list = nextBackups.value;
      setBackups(list);
      setArchive((chosen) =>
        list.some((b) => b.name === chosen) ? chosen : (list[0]?.name ?? ""),
      );
    }
    if (failures.length > 0) setError(describe(failures[0], onSessionError));
  }, [onSessionError]);

  // Follows one job until it ends, then refreshes the lists.
  const follow = useCallback(
    (id: string) => {
      const signal = followRef.current?.signal;
      const again = (ms: number) => {
        pollRef.current = globalThis.setTimeout(() => void tick(), ms);
      };
      const tick = async () => {
        pollRef.current = null;
        try {
          const job = await serverWriteApi.job(id, signal);
          if (signal?.aborted) return;
          setCurrent(job);
          setJobs((list) => list.map((j) => (j.id === job.id ? job : j)));
          if (job.state === "running") {
            again(pollMs);
            return;
          }
          setStatus(
            job.state === "succeeded"
              ? ""
              : "The action failed. Its output is below.",
          );
          void refresh();
        } catch (cause) {
          if (signal?.aborted) return;
          if (transient(cause)) {
            const wait = (cause as ApiError).retryAfter ?? 0;
            again(Math.max(pollMs * 2, wait * 1000));
            return;
          }
          // Stop locking on a snapshot that can no longer update. The jobs
          // check picks the job up again if it is still running.
          setCurrent((now) => (now?.id === id ? null : now));
          setError(describe(cause, onSessionError));
        }
      };
      void tick();
    },
    [onSessionError, pollMs, refresh],
  );

  useEffect(() => {
    followRef.current = new AbortController();
    return () => {
      followRef.current?.abort();
      if (pollRef.current !== null) globalThis.clearTimeout(pollRef.current);
      pollRef.current = null;
    };
  }, []);

  useEffect(() => {
    void refresh();
    const timer = globalThis.setInterval(() => {
      if (pollRef.current === null) void refresh();
    }, pollMs * JOBS_EVERY);
    return () => {
      abortRef.current?.abort();
      globalThis.clearInterval(timer);
    };
  }, [refresh, pollMs]);

  // A job started elsewhere (another tab or admin) is followed too.
  const running = jobs.find((job) => job.state === "running");
  useEffect(() => {
    if (running && pollRef.current === null && current?.id !== running.id) {
      follow(running.id);
    }
  }, [running, current, follow]);

  const run = async (action: WriteAction, args: ServerActionArguments = {}) => {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      const body: Record<string, unknown> = { ...args };
      setStatus("Touch your security key to confirm");
      const challenge = await serverWriteApi.stepUp(action, args);
      const credential = await navigator.credentials.get({
        publicKey: decodeRequestOptions(challenge.publicKey),
      });
      if (!credential)
        throw new DOMException("no credential", "NotAllowedError");
      body.stepup = {
        handle: challenge.handle,
        assertion: encodeAssertion(credential as PublicKeyCredential),
      };
      setStatus(`${ACTION_LABELS[action]} started`);
      const job = await serverWriteApi.run(action, body);
      setCurrent(job);
      follow(job.id);
    } catch (cause) {
      setStatus("");
      if (cause instanceof ServerRequestError && cause.appliance) {
        setAppliance(cause.appliance);
        return;
      }
      setError(describe(cause, onSessionError));
      // Someone else started a job: load it so the buttons lock.
      if (
        cause instanceof ServerRequestError &&
        cause.code === "write_in_progress"
      )
        void refresh();
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

  const locked = busy || running !== undefined || current?.state === "running";
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
            One action runs at a time. Every action asks for your security key
            and is audited.
          </p>
        </div>
      </div>
      {/* Above the controls: a refusal can come from any of them, so it must
          not sit under the restore form. */}
      {status && (
        <output className="resource-notice resource-loading">{status}</output>
      )}
      {error && <output className="resource-notice">{error}</output>}
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
