export function bypassesSteerWhenRunning(trimmed: string): boolean {
  return /^\/doctor$/.test(trimmed);
}
