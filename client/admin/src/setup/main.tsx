import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import "../styles.css";
import { SetupWizard } from "./SetupWizard";

const root = document.getElementById("root");

if (!root) {
  throw new Error("Setup root is unavailable");
}

createRoot(root).render(
  <StrictMode>
    <SetupWizard />
  </StrictMode>,
);
