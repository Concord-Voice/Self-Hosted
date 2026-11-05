import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

import { parseServerHealth, parseServerStatus } from "./server";
import { ServerWorkspace } from "./ServerWorkspace";
import { server } from "./test/server";

const base = "http://localhost/admin/api/v1/server";

const statusBody = {
  schema: 1,
  services: [
    {
      service: "postgres",
      name: "concord-postgres-1",
      state: "running",
      health: "healthy",
      image: "postgres:16",
      exit_code: null,
    },
    {
      service: "redis",
      name: "concord-redis-1",
      state: "exited",
      health: "none",
      image: "redis:8",
      exit_code: 1,
    },
  ],
};
const healthBody = {
  schema: 1,
  checks: [{ name: "control-plane", ok: false }],
  ok: false,
};

function agentUp() {
  server.use(
    http.get(`${base}/status`, () => HttpResponse.json(statusBody)),
    http.get(`${base}/health`, () => HttpResponse.json(healthBody)),
  );
}

function renderWorkspace() {
  const onForbidden = vi.fn();
  const onUnauthorized = vi.fn();
  render(
    <ServerWorkspace
      onForbidden={onForbidden}
      onUnauthorized={onUnauthorized}
    />,
  );
  return { onForbidden, onUnauthorized };
}

describe("ServerWorkspace", () => {
  it("shows containers and health checks from the agent", async () => {
    agentUp();
    renderWorkspace();
    expect(
      await screen.findByRole("rowheader", { name: "postgres" }),
    ).toBeVisible();
    expect(screen.getByText("exited (exit 1)")).toBeVisible();
    expect(screen.getByText("At least one check failed")).toBeVisible();
    expect(
      screen.getByText("control-plane", { selector: ".server-checks span" }),
    ).toBeVisible();
  });

  it("explains an unreachable agent", async () => {
    server.use(
      http.get(`${base}/status`, () =>
        HttpResponse.json({ error: "agent_unavailable" }, { status: 503 }),
      ),
      http.get(`${base}/health`, () =>
        HttpResponse.json({ error: "agent_unavailable" }, { status: 503 }),
      ),
    );
    renderWorkspace();
    expect(
      await screen.findByText(/host agent is not reachable/),
    ).toBeVisible();
  });

  it("ends the session on 401 and hands 403 to the portal", async () => {
    server.use(
      http.get(`${base}/status`, () => new HttpResponse(null, { status: 401 })),
      http.get(`${base}/health`, () => new HttpResponse(null, { status: 401 })),
    );
    const first = renderWorkspace();
    await waitFor(() => expect(first.onUnauthorized).toHaveBeenCalled());
  });

  it("loads logs for the chosen service and line count", async () => {
    agentUp();
    let query = "";
    server.use(
      http.get(`${base}/logs`, ({ request }) => {
        query = new URL(request.url).search;
        return HttpResponse.text("first line\nsecond line\n");
      }),
    );
    renderWorkspace();
    const user = userEvent.setup();
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Service" }),
      "redis",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Lines" }),
      "500",
    );
    await user.click(screen.getByRole("button", { name: "Show logs" }));
    expect(await screen.findByLabelText("redis logs")).toHaveTextContent(
      "first line",
    );
    expect(query).toBe("?service=redis&lines=500");
  });

  it("reports a busy agent for logs", async () => {
    agentUp();
    server.use(
      http.get(`${base}/logs`, () => new HttpResponse(null, { status: 429 })),
    );
    renderWorkspace();
    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "Show logs" }));
    expect(await screen.findByText(/host agent is busy/)).toBeVisible();
  });

  it("hands 403 from logs to the portal", async () => {
    agentUp();
    server.use(
      http.get(`${base}/logs`, () => new HttpResponse(null, { status: 403 })),
    );
    const { onForbidden } = renderWorkspace();
    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "Show logs" }));
    await waitFor(() => expect(onForbidden).toHaveBeenCalled());
  });
});

describe("server parsers", () => {
  it("accept the documented shapes", () => {
    expect(parseServerStatus(statusBody).services).toHaveLength(2);
    expect(parseServerHealth(healthBody).ok).toBe(false);
  });

  it.each([
    null,
    { schema: 2, services: [] },
    { schema: 1, services: [{ service: "x" }] },
    { schema: 1, services: [{ ...statusBody.services[0], exit_code: "1" }] },
  ])("reject status %j", (value) => {
    expect(() => parseServerStatus(value)).toThrow("did not match");
  });

  it.each([
    { schema: 1, checks: [], ok: "yes" },
    { schema: 1, checks: [{ name: 1, ok: true }], ok: true },
    { schema: 1, ok: true },
  ])("reject health %j", (value) => {
    expect(() => parseServerHealth(value)).toThrow("did not match");
  });
});
