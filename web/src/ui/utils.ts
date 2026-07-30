import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/** Compare configuration arrays as sets without mutating their source order. */
export function sameMembers(left: readonly string[], right: readonly string[]) {
  return [...left].sort().join("\0") === [...right].sort().join("\0");
}
