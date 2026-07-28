/** Reviewer selections are sets; UI toggle order is not a configuration change. */
export function sameSet(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value) => right.includes(value));
}

export function setKey(values: readonly string[]): string {
  return JSON.stringify([...values].sort());
}
