import { useEffect } from "react";

import { MAX_ADMINS_ADDED_NOTICE } from "./api";

interface AdminsAddedNoticeProps {
  count: number;
  // cursor acknowledges this notice; null when there is nothing to
  // acknowledge.
  cursor: string | null;
  onDismiss: () => void;
  // onDisplayed is called with the cursor once the notice is on screen.
  onDisplayed: (cursor: string) => void;
}

// Fixed copy around the one validated scalar the sign-in response carries.
export function adminsAddedMessage(count: number): string {
  if (count >= MAX_ADMINS_ADDED_NOTICE) {
    return `${MAX_ADMINS_ADDED_NOTICE} or more admins were added in this console since your last sign-in.`;
  }
  if (count === 1) {
    return "1 admin was added in this console since your last sign-in.";
  }
  return `${count} admins were added in this console since your last sign-in.`;
}

// Tells a signed-in admin that admins were added since the last notice they
// saw, so an admin added with a stolen session and key touch is noticed. Once
// it is displayed it reports its cursor, which the server needs before it
// stops showing these admins at later sign-ins.
export function AdminsAddedNotice({
  count,
  cursor,
  onDismiss,
  onDisplayed,
}: Readonly<AdminsAddedNoticeProps>) {
  const shown = count > 0;
  useEffect(() => {
    if (shown && cursor !== null) onDisplayed(cursor);
  }, [shown, cursor, onDisplayed]);

  if (!shown) return null;
  return (
    <section
      aria-labelledby="admins-added-title"
      className="shell-notice admins-added-notice"
    >
      <h2 id="admins-added-title">
        <span aria-hidden="true">!</span> Admins added
      </h2>
      <p role="alert">{adminsAddedMessage(count)}</p>
      <p>If you did not expect this, check the admin audit log.</p>
      <button className="server-action" type="button" onClick={onDismiss}>
        Dismiss
      </button>
    </section>
  );
}
