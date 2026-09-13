import { isWailsHosted } from "./desktopBridge";
import { isScopedPane } from "./scope";

export const WORKSPACE_SWITCH_REQUEST = "workspace-switch";
export const WORKSPACE_SWITCH_RESULT = "workspace-switch-result";

interface WorkspaceSwitchWindow {
  parent: {
    postMessage: (message: unknown, targetOrigin: string) => void;
  };
  location: { origin: string };
  addEventListener: (
    type: "message",
    listener: (event: MessageEvent) => void,
  ) => void;
  removeEventListener: (
    type: "message",
    listener: (event: MessageEvent) => void,
  ) => void;
  setTimeout: (handler: () => void, timeout: number) => number;
  clearTimeout: (id: number) => void;
}

interface WorkspaceSwitchOptions {
  currentWindow?: WorkspaceSwitchWindow;
  scoped?: boolean;
  wailsHosted?: boolean;
  requestId?: string;
  timeoutMs?: number;
}

function generatedRequestId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

/**
 * After the scoped project API has accepted a switch, ask the browser
 * workspace shell to display the matching iframe. Wails panes keep using the
 * desktop shell's own navigation and standalone Studio has no parent shell,
 * so both settle immediately.
 */
export function requestBrowserWorkspaceSwitch(
  projectId: string,
  options: WorkspaceSwitchOptions = {},
): Promise<void> {
  const currentWindow =
    options.currentWindow ??
    (typeof window !== "undefined" ? (window as unknown as WorkspaceSwitchWindow) : undefined);
  const scoped = options.scoped ?? isScopedPane();
  const wailsHosted = options.wailsHosted ?? isWailsHosted();

  if (
    !currentWindow ||
    !scoped ||
    wailsHosted ||
    currentWindow.parent === (currentWindow as unknown)
  ) {
    return Promise.resolve();
  }

  const requestId = options.requestId ?? generatedRequestId();
  const timeoutMs = options.timeoutMs ?? 10_000;

  return new Promise<void>((resolve, reject) => {
    let timer = 0;
    const cleanup = () => {
      currentWindow.removeEventListener("message", onMessage);
      currentWindow.clearTimeout(timer);
    };
    const onMessage = (event: MessageEvent) => {
      if (
        event.origin !== currentWindow.location.origin ||
        event.source !== (currentWindow.parent as unknown) ||
        !event.data ||
        event.data.source !== "iterion-shell" ||
        event.data.type !== WORKSPACE_SWITCH_RESULT ||
        event.data.requestId !== requestId
      ) {
        return;
      }
      cleanup();
      if (event.data.ok === true) {
        resolve();
        return;
      }
      reject(new Error(event.data.error || "The workspace could not display this project"));
    };

    currentWindow.addEventListener("message", onMessage);
    timer = currentWindow.setTimeout(() => {
      cleanup();
      reject(new Error("The workspace did not confirm the project switch"));
    }, timeoutMs);
    currentWindow.parent.postMessage(
      {
        source: "iterion-pane",
        type: WORKSPACE_SWITCH_REQUEST,
        projectId,
        requestId,
      },
      currentWindow.location.origin,
    );
  });
}
