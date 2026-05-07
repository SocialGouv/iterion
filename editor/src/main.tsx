import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "@xyflow/react/dist/style.css";
import App from "./App";
import "./app.css";
import { initializeTheme } from "./store/theme";
import { initializeBackendDetect } from "./store/backendDetect";

initializeTheme();
initializeBackendDetect();

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
