import { AlertTriangle, RotateCcw } from "lucide-react";
import { Component, type ErrorInfo, type ReactNode } from "react";

type Props = { children: ReactNode };
type State = { error: Error | null };

/** Last-resort render boundary: a defect must become visible, never a blank page. */
export class AppErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  override componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("Dashboard render defect", error, info.componentStack);
  }

  override render() {
    if (!this.state.error) return this.props.children;
    return (
      <main className="mx-auto flex min-h-screen max-w-2xl items-center px-6 py-16">
        <section className="w-full rounded-xl border border-bad-edge bg-card p-6 shadow-card">
          <AlertTriangle aria-hidden className="mb-4 size-6 text-bad" />
          <h1 className="text-lg font-[650]">The dashboard hit an unexpected error</h1>
          <p className="mt-2 text-sm text-mut">
            The queue keeps running. Reload this view; if it repeats, the detail below is the defect
            to inspect.
          </p>
          <pre className="mt-4 max-h-48 overflow-auto rounded-lg bg-ink p-3 text-xs text-white/80">
            {this.state.error.message}
          </pre>
          <button
            type="button"
            onClick={() => location.reload()}
            className="mt-4 inline-flex items-center gap-2 rounded-lg bg-ink px-4 py-2 text-sm font-semibold text-white"
          >
            <RotateCcw aria-hidden className="size-4" />
            Reload dashboard
          </button>
        </section>
      </main>
    );
  }
}
