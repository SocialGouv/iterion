import { Component, type ErrorInfo, type ReactNode } from "react";

interface Props {
  /** A label used in the fallback UI so a user can tell which area
   *  crashed — e.g. "Run view", "Launch view". */
  area: string;
  children: ReactNode;
  /** When set, the boundary resets its error state whenever this value
   *  changes. Used to recover automatically when the user navigates to
   *  a different run id without forcing a full page reload. */
  resetKey?: string | number | null;
  /** Replaces the default inline card. Use it where the built-in
   *  fallback is the wrong shape — a crashed corner dock should vanish
   *  (`null`), and a crashed shell-level PROVIDER should re-render its
   *  subtree without itself rather than show an error where the whole
   *  app used to be. */
  fallback?: ReactNode;
}

interface State {
  error: Error | null;
}

// ErrorBoundary catches render-phase exceptions in its subtree so a
// single component bug (e.g. a malformed event payload that crashes a
// reducer-derived render path) doesn't leave the user staring at a
// blank page. Renders a small inline fallback with a "Try again"
// button that clears the captured error; navigating away (which
// changes `resetKey`) also clears it.
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error(`[${this.props.area}] render error:`, error, info);
  }

  componentDidUpdate(prev: Props): void {
    if (this.state.error && prev.resetKey !== this.props.resetKey) {
      this.setState({ error: null });
    }
  }

  private handleRetry = () => {
    this.setState({ error: null });
  };

  render(): ReactNode {
    if (!this.state.error) return this.props.children;
    if (this.props.fallback !== undefined) return this.props.fallback;
    return (
      <div className="h-full flex items-center justify-center p-6 bg-surface-0">
        <div className="max-w-md w-full rounded border border-danger/40 bg-danger-soft p-4 text-danger-fg">
          <div className="text-sm font-medium mb-1">
            {this.props.area} crashed
          </div>
          <div className="text-xs font-mono break-words whitespace-pre-wrap mb-3">
            {this.state.error.message || String(this.state.error)}
          </div>
          <button
            type="button"
            onClick={this.handleRetry}
            className="text-xs px-2 py-1 rounded bg-surface-1 hover:bg-surface-2 border border-border-default text-fg-default"
          >
            Try again
          </button>
        </div>
      </div>
    );
  }
}
