// Backoff is a capped exponential backoff: the first delay, the factor each
// later delay grows by, and the delay it stops growing at.
export interface Backoff {
  firstMs: number;
  factor: number;
  capMs: number;
}

// backoffDelay is the delay before attempt n, counted from 0.
export function backoffDelay(backoff: Backoff, attempt: number): number {
  return Math.min(backoff.capMs, backoff.firstMs * backoff.factor ** attempt);
}

export interface BackoffLoop {
  // cancel ends the loop: a pending attempt never runs, and the answer of
  // one in flight re-arms nothing.
  cancel(): void;
}

// startBackoffLoop runs attempt after backoffDelay(0), and again after each
// "retry" with the next delay, until it answers "done" or the loop is
// cancelled. One attempt runs at a time — the next is armed only once the
// previous answered — and onEnd runs exactly once, however the loop ends. An
// attempt that throws ends the loop and the error propagates.
export function startBackoffLoop(
  backoff: Backoff,
  attempt: () => Promise<"done" | "retry">,
  onEnd: () => void,
): BackoffLoop {
  let ended = false;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let attempts = 0;
  const end = () => {
    if (ended) return;
    ended = true;
    if (timer !== null) clearTimeout(timer);
    timer = null;
    onEnd();
  };
  const arm = () => {
    timer = setTimeout(() => {
      timer = null;
      void attempt().then(
        (step) => {
          if (ended) return;
          if (step === "done") {
            end();
            return;
          }
          attempts++;
          arm();
        },
        (err: unknown) => {
          end();
          throw err;
        },
      );
    }, backoffDelay(backoff, attempts));
  };
  arm();
  return { cancel: end };
}
