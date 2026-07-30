import { useEffect, useState } from "react";

/**
 * A clock that ticks locally between server pushes. Snapshots arrive only when
 * the state ref moves, so every countdown on screen is computed from this.
 */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
}

/** Absolute wall-clock time, the format the Markdown dashboard also uses. */
export function clock(iso?: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

function hms(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  const mm = h > 0 ? String(m).padStart(2, "0") : String(m);
  return `${h > 0 ? `${h}:` : ""}${mm}:${String(sec).padStart(2, "0")}`;
}

/**
 * Time remaining until `iso`. A countdown that reaches zero says the work is
 * due rather than going negative — the daemon acts on its own cadence, and
 * pretending otherwise makes every other number on the page less trustworthy.
 */
export function countdown(iso: string | undefined, now: number): string {
  if (!iso) return "—";
  const left = (new Date(iso).getTime() - now) / 1000;
  if (left <= 0) return "due";
  return `−${hms(left)}`;
}

/** Time elapsed since `iso`, for things that are running right now. */
export function elapsed(iso: string | undefined, now: number): string {
  if (!iso) return "—";
  return hms((now - new Date(iso).getTime()) / 1000);
}

/** Coarse "3m ago" for timestamps where precision would be noise. */
export function ago(iso: string | undefined, now: number): string {
  if (!iso) return "—";
  const s = Math.max(0, (now - new Date(iso).getTime()) / 1000);
  if (s < 90) return `${Math.round(s)}s ago`;
  if (s < 5400) return `${Math.round(s / 60)}m ago`;
  if (s < 172800) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}
