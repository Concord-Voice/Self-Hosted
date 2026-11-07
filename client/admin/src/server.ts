import { ApiContractError, ApiError, retryAfter } from "./api";

// Read-only Server area (#186). The control plane forwards these to the
// Concord Host Agent, which allows only these services and line counts.
export const LOG_SERVICES = [
  "control-plane",
  "media-plane",
  "postgres",
  "redis",
  "nats",
  "minio",
  "coturn",
] as const;
export type LogService = (typeof LOG_SERVICES)[number];
export const LOG_LINE_CHOICES = [100, 200, 500, 1000] as const;
export type LogLineChoice = (typeof LOG_LINE_CHOICES)[number];

export interface ServerService {
  exit_code: number | null;
  health: string;
  image: string;
  name: string;
  service: string;
  state: string;
}
export interface ServerStatus {
  services: ServerService[];
}
export interface ServerHealth {
  checks: { name: string; ok: boolean }[];
  ok: boolean;
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

function ensure(condition: boolean): asserts condition {
  if (!condition) throw new ApiContractError();
}

export function parseServerStatus(value: unknown): ServerStatus {
  ensure(
    isRecord(value) && value.schema === 1 && Array.isArray(value.services),
  );
  const services = (value.services as unknown[]).map((item) => {
    ensure(isRecord(item));
    for (const key of ["service", "name", "state", "health", "image"]) {
      ensure(typeof item[key] === "string");
    }
    ensure(item.exit_code === null || Number.isInteger(item.exit_code));
    return item as unknown as ServerService;
  });
  return { services };
}

export function parseServerHealth(value: unknown): ServerHealth {
  ensure(
    isRecord(value) && value.schema === 1 && typeof value.ok === "boolean",
  );
  ensure(Array.isArray(value.checks));
  const checks = (value.checks as unknown[]).map((item) => {
    ensure(
      isRecord(item) &&
        typeof item.name === "string" &&
        typeof item.ok === "boolean",
    );
    return { name: item.name as string, ok: item.ok as boolean };
  });
  return { checks, ok: value.ok as boolean };
}

async function get(path: string, signal?: AbortSignal): Promise<Response> {
  let response: Response;
  try {
    response = await fetch(path, { credentials: "same-origin", signal });
  } catch {
    throw new ApiError(0, null);
  }
  if (!response.ok) {
    throw new ApiError(
      response.status,
      retryAfter(response.headers.get("Retry-After")),
    );
  }
  return response;
}

async function getJSON<T>(
  path: string,
  parse: (value: unknown) => T,
  signal?: AbortSignal,
): Promise<T> {
  const response = await get(path, signal);
  let value: unknown;
  try {
    value = await response.json();
  } catch {
    throw new ApiContractError();
  }
  return value;
}

export const serverApi = {
  status: (signal?: AbortSignal) =>
    getJSON("/admin/api/v1/server/status", parseServerStatus, signal),
  health: (signal?: AbortSignal) =>
    getJSON("/admin/api/v1/server/health", parseServerHealth, signal),
  async logs(service: LogService, lines: LogLineChoice, signal?: AbortSignal) {
    const query = new URLSearchParams({ service, lines: String(lines) });
    const response = await get(
      `/admin/api/v1/server/logs?${query.toString()}`,
      signal,
    );
    return response.text();
  },
};

// Write actions (#188). Stop, restart, update and restore need a fresh
// security-key assertion; the control plane checks it and the host agent
// re-checks every argument.
export const WRITE_ACTIONS = [
  "start",
  "stop",
  "restart",
  "backup",
  "update",
  "restore",
] as const;
export type WriteAction = (typeof WRITE_ACTIONS)[number];
export const STEP_UP_ACTIONS: ReadonlySet<WriteAction> = new Set([
  "stop",
  "restart",
  "update",
  "restore",
]);
// The same shapes the control plane and agent accept.
export const UPDATE_TAG = /^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$/;
const ARCHIVE = /^concord-backup-\d{8}T\d{6}Z\.tar\.gz$/;
const JOB_ID = /^[0-9a-f]{32}$/;
const JOB_STATES = ["running", "succeeded", "failed"] as const;
export const APPLIANCES = { truenas: "TrueNAS" } as const;
export type Appliance = keyof typeof APPLIANCES;

export interface ServerJob {
  id: string;
  action: WriteAction;
  state: (typeof JOB_STATES)[number];
  started: string;
  finished?: string;
  archive?: string;
  output?: string;
}
export interface ServerBackup {
  name: string;
  size: number;
  modified: string;
}

// A refused request with the reason the server gave, if it gave a known one.
export class ServerRequestError extends ApiError {
  constructor(
    status: number,
    public readonly code: string | null,
    public readonly appliance: Appliance | null = null,
  ) {
    super(status, null);
    this.name = "ServerRequestError";
  }
}

// Go writes UTC times as RFC 3339 with a Z.
const UTC_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/;
const isTime = (v: unknown) =>
  typeof v === "string" && UTC_TIME.test(v) && !Number.isNaN(Date.parse(v));

export function parseServerJob(value: unknown): ServerJob {
  ensure(isRecord(value));
  ensure(typeof value.id === "string" && JOB_ID.test(value.id));
  ensure(WRITE_ACTIONS.includes(value.action as WriteAction));
  ensure(JOB_STATES.includes(value.state as ServerJob["state"]));
  ensure(isTime(value.started));
  ensure(value.finished === undefined || isTime(value.finished));
  ensure(
    value.archive === undefined ||
      (typeof value.archive === "string" && ARCHIVE.test(value.archive)),
  );
  ensure(value.output === undefined || typeof value.output === "string");
  return value as unknown as ServerJob;
}

export function parseServerJobs(value: unknown): ServerJob[] {
  ensure(isRecord(value) && Array.isArray(value.jobs));
  return (value.jobs as unknown[]).map(parseServerJob);
}

export function parseServerBackups(value: unknown): ServerBackup[] {
  ensure(isRecord(value) && Array.isArray(value.backups));
  return (value.backups as unknown[]).map((item) => {
    ensure(
      isRecord(item) &&
        typeof item.name === "string" &&
        ARCHIVE.test(item.name) &&
        Number.isInteger(item.size) &&
        isTime(item.modified),
    );
    return item as unknown as ServerBackup;
  });
}

async function send(
  path: string,
  init: { body?: unknown; signal?: AbortSignal },
): Promise<unknown> {
  let response: Response;
  try {
    response = await fetch(path, {
      method: init.body === undefined ? "GET" : "POST",
      credentials: "same-origin",
      headers:
        init.body === undefined
          ? undefined
          : { "Content-Type": "application/json" },
      body: init.body === undefined ? undefined : JSON.stringify(init.body),
      signal: init.signal,
    });
  } catch {
    throw new ApiError(0, null);
  }
  let value: unknown = null;
  try {
    value = await response.json();
  } catch {
    if (response.ok) throw new ApiContractError();
  }
  if (!response.ok) {
    const code =
      isRecord(value) && typeof value.error === "string" ? value.error : null;
    const appliance =
      isRecord(value) && Object.hasOwn(APPLIANCES, String(value.appliance))
        ? (value.appliance as Appliance)
        : null;
    throw new ServerRequestError(response.status, code, appliance);
  }
  return value;
}

export const serverWriteApi = {
  jobs: (signal?: AbortSignal) =>
    send("/admin/api/v1/server/jobs", { signal }).then(parseServerJobs),
  job: (id: string, signal?: AbortSignal) =>
    send(`/admin/api/v1/server/jobs/${encodeURIComponent(id)}`, {
      signal,
    }).then(parseServerJob),
  backups: (signal?: AbortSignal) =>
    send("/admin/api/v1/server/backups", { signal }).then(parseServerBackups),
  async stepUp(action: WriteAction) {
    const value = await send("/admin/api/v1/server/stepup", {
      body: { action },
    });
    ensure(
      isRecord(value) &&
        typeof value.handle === "string" &&
        isRecord(value.publicKey),
    );
    return { handle: value.handle as string, publicKey: value.publicKey };
  },
  run: (action: WriteAction, body: Record<string, unknown>) =>
    send(`/admin/api/v1/server/actions/${action}`, { body }).then(
      parseServerJob,
    ),
};
