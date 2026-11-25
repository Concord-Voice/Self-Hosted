import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { adminsAddedMessage } from "./AdminsAddedNotice";
import { App } from "./App";
import type { SignInNotice } from "./api";
import {
  countersFixture,
  currentFixture,
  healthFixture,
  seriesFixture,
} from "./test/fixtures";
import { server } from "./test/server";

// The real AuthScreen is covered in AuthScreen.test.tsx. Here each click is
// one completed sign-in that reports the next notice in the queue.
const signIns: SignInNotice[] = [];
vi.mock("./AuthScreen", () => ({
  AuthScreen: ({
    onAuthenticated,
  }: {
    onAuthenticated: (notice: SignInNotice) => void;
  }) => (
    <button
      type="button"
      onClick={() =>
        onAuthenticated(signIns.shift() ?? { adminsAdded: 0, cursor: null })
      }
    >
      Complete sign-in
    </button>
  ),
}));

const base = "http://localhost";

function notice(adminsAdded: number, cursor: string): SignInNotice {
  return { adminsAdded, cursor };
}

// Records each acknowledgement the portal sends, answering with status.
function serveAck(status = 204): unknown[] {
  const acks: unknown[] = [];
  server.use(
    http.post(`${base}/admin/api/v1/signin-notice/ack`, async ({ request }) => {
      acks.push(await request.json());
      return status === 204
        ? new HttpResponse(null, { status })
        : HttpResponse.json({ error: "invalid_cursor" }, { status });
    }),
  );
  return acks;
}

// Health answers 401 until a sign-in completes, then 200; the probe after the
// first sign-in can also be made to 401 once, as if the session were lost.
function serveHealth(failFirstProbeAfterSignIn = false) {
  let signedIn = false;
  let failNext = failFirstProbeAfterSignIn;
  server.use(
    http.get(`${base}/admin/api/v1/health`, () => {
      if (!signedIn) return HttpResponse.json({}, { status: 401 });
      if (failNext) {
        failNext = false;
        return HttpResponse.json({}, { status: 401 });
      }
      return HttpResponse.json(healthFixture());
    }),
    http.post(`${base}/admin/api/v1/auth/logout`, () =>
      HttpResponse.json({ status: "logged out" }),
    ),
  );
  return () => {
    signedIn = true;
  };
}

// Waits for the login view first, so the probe that showed it saw no session.
async function signIn(markSignedIn: () => void) {
  const button = await screen.findByRole("button", {
    name: "Complete sign-in",
  });
  markSignedIn();
  await userEvent.click(button);
}

describe("admins-added sign-in notice", () => {
  beforeEach(() => {
    history.replaceState(null, "", "/admin/");
    signIns.length = 0;
    server.use(
      http.get(`${base}/admin/api/v1/metrics/current`, () =>
        HttpResponse.json(currentFixture()),
      ),
      http.get(`${base}/admin/api/v1/counters`, () =>
        HttpResponse.json(countersFixture()),
      ),
      http.get(`${base}/admin/api/v1/metrics/series`, () =>
        HttpResponse.json(seriesFixture()),
      ),
    );
  });

  it("shows the count after sign-in until it is dismissed", async () => {
    const markSignedIn = serveHealth();
    serveAck();
    signIns.push(notice(3, "41"));
    render(<App />);
    await signIn(markSignedIn);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "3 admins were added in this console since your last sign-in.",
    );
    expect(screen.getByRole("heading", { name: "Admins added" })).toBeVisible();

    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(
      screen.queryByRole("heading", { name: "Admins added" }),
    ).not.toBeInTheDocument();
  });

  it("acknowledges the cursor once the notice is displayed", async () => {
    const markSignedIn = serveHealth();
    const acks = serveAck();
    signIns.push(notice(2, "917"));
    render(<App />);
    await signIn(markSignedIn);

    expect(await screen.findByRole("alert")).toBeVisible();
    await waitFor(() => expect(acks).toEqual([{ cursor: "917" }]));
    // Dismissing sends nothing more.
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(acks).toHaveLength(1);
  });

  it("acknowledges nothing before the portal shows the notice", async () => {
    const markSignedIn = serveHealth(true);
    const acks = serveAck();
    signIns.push(notice(2, "50"), notice(3, "60"));
    render(<App />);
    await signIn(markSignedIn);
    // The probe after the first sign-in was refused, so the login view is back
    // and the first notice was never displayed.
    await screen.findByRole("button", { name: "Complete sign-in" });
    expect(acks).toEqual([]);
    await signIn(markSignedIn);

    // The second sign-in's count already includes the first one's admins.
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "3 admins were added in this console since your last sign-in.",
    );
    await waitFor(() => expect(acks).toEqual([{ cursor: "60" }]));
  });

  it("keeps the notice on screen when the acknowledgement fails", async () => {
    const markSignedIn = serveHealth();
    const acks = serveAck(400);
    signIns.push(notice(1, "7"));
    render(<App />);
    await signIn(markSignedIn);

    await waitFor(() => expect(acks).toHaveLength(1));
    expect(screen.getByRole("alert")).toHaveTextContent(
      "1 admin was added in this console since your last sign-in.",
    );
  });

  it("shows nothing and acknowledges nothing when no admin was added", async () => {
    const markSignedIn = serveHealth();
    const acks = serveAck();
    signIns.push({ adminsAdded: 0, cursor: null });
    render(<App />);
    await signIn(markSignedIn);

    expect(
      await screen.findByRole("heading", { name: "Host Overview" }),
    ).toBeVisible();
    expect(
      screen.queryByRole("heading", { name: "Admins added" }),
    ).not.toBeInTheDocument();
    expect(acks).toEqual([]);
  });

  it("clears the notice when the session ends", async () => {
    const markSignedIn = serveHealth();
    const acks = serveAck();
    signIns.push(notice(1, "5"), { adminsAdded: 0, cursor: null });
    render(<App />);
    await signIn(markSignedIn);
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "1 admin was added in this console since your last sign-in.",
    );

    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await signIn(markSignedIn);

    expect(
      await screen.findByRole("heading", { name: "Host Overview" }),
    ).toBeVisible();
    await waitFor(() =>
      expect(
        screen.queryByRole("heading", { name: "Admins added" }),
      ).not.toBeInTheDocument(),
    );
    expect(acks).toEqual([{ cursor: "5" }]);
  });

  it("acknowledges the same cursor again in a new session", async () => {
    const markSignedIn = serveHealth();
    const acks = serveAck(503);
    signIns.push(notice(1, "8"), notice(1, "8"));
    render(<App />);
    await signIn(markSignedIn);
    await waitFor(() => expect(acks).toHaveLength(1));

    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await signIn(markSignedIn);
    await waitFor(() =>
      expect(acks).toEqual([{ cursor: "8" }, { cursor: "8" }]),
    );
  });

  it.each([
    [1, "1 admin was added in this console since your last sign-in."],
    [99, "99 admins were added in this console since your last sign-in."],
    [
      100,
      "100 or more admins were added in this console since your last sign-in.",
    ],
  ])("words a count of %i", (count, message) => {
    expect(adminsAddedMessage(count)).toBe(message);
  });
});
