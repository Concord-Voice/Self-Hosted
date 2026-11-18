import { MAX_ADMINS_ADDED_NOTICE } from "./api";

interface AdminsAddedNoticeProps {
  count: number;
  onDismiss: () => void;
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

// Tells a signed-in admin that admins were created since their previous
// sign-in, so an admin added with a stolen session and key touch is noticed.
export function AdminsAddedNotice({
  count,
  onDismiss,
}: Readonly<AdminsAddedNoticeProps>) {
  return (
    <section
      aria-labelledby="admins-added-title"
      className="shell-notice admins-added-notice"
    >
      <h2 id="admins-added-title">
        <span aria-hidden="true">! </span>
        Admins added
      </h2>
      <p role="alert">{adminsAddedMessage(count)}</p>
      <p>If you did not expect this, check the admin audit log.</p>
      <button type="button" onClick={onDismiss}>
        Dismiss
      </button>
    </section>
  );
}
