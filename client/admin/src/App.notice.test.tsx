import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { adminsAddedMessage } from "./AdminsAddedNotice";
import { App } from "./App";
import {
  countersFixture,
  currentFixture,
  healthFixture,
  seriesFixture,
} from "./test/fixtures";
import { server } from "./test/server";

// The real AuthScreen is covered in AuthScreen.test.tsx. Here each click is
// one completed sign-in that reports the next count in the queue.
const signInCounts: number[] = [];
vi.mock("./AuthScreen", () => ({
  AuthScreen: ({
    onAuthenticated,
  }: {
    onAuthenticated: (adminsAdded: number) => void;
  }) => (
    <button
      type="button"
      onClick={() => onAuthenticated(signInCounts.shift() ?? 0)}
    >
      Complete sign-in
    </button>
  ),
}));

const base = "http://localhost";

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
    signInCounts.length = 0;
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
    signInCounts.push(3);
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

  it("shows nothing when no admin was added", async () => {
    const markSignedIn = serveHealth();
    signInCounts.push(0);
    render(<App />);
    await signIn(markSignedIn);

    expect(
      await screen.findByRole("heading", { name: "Host Overview" }),
    ).toBeVisible();
    expect(
      screen.queryByRole("heading", { name: "Admins added" }),
    ).not.toBeInTheDocument();
  });

  it("keeps an undelivered count when the portal needs a second sign-in", async () => {
    const markSignedIn = serveHealth(true);
    signInCounts.push(2, 0);
    render(<App />);
    await signIn(markSignedIn);
    // The probe after the first sign-in was refused, so the login view is back.
    await signIn(markSignedIn);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "2 admins were added in this console since your last sign-in.",
    );
  });

  it("clears the notice when the session ends", async () => {
    const markSignedIn = serveHealth();
    signInCounts.push(1, 0);
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
