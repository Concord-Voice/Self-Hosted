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
  return parse(value);
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
