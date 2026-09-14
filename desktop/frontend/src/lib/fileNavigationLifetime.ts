let controller = new AbortController();

export function cancelFileNavigation(): void {
  controller.abort();
}

export function beginFileNavigation(): AbortSignal {
  cancelFileNavigation();
  controller = new AbortController();
  return controller.signal;
}
