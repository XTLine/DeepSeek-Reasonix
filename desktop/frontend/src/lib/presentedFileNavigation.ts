import { app } from "./bridge";
import { useBrowserPanelStore, waitForBrowserHost } from "./browserPanelStore";
import { useActivityBarStore } from "../store/activityBar";
import { useLayoutStore } from "../store/layout";
import { useRemoteStore } from "../store/remote";
import { beginFileNavigation, cancelFileNavigation } from "./fileNavigationLifetime";

type ResourceBase = { hostId: string; tabId: string; path: string };
export type FileResourceRef =
  | (ResourceBase & { source: "presented"; toolCallId: string })
  | (ResourceBase & { source: "workspace"; toolCallId: string });

export type PresentedFileAction = "preview" | "browser" | "reveal-tree" | "source" | "open-native" | "reveal-native" | "save-copy";
export type PresentedFileRequest = { id: number; ref: FileResourceRef; action: "preview" | "reveal-tree" | "source"; dockTabId: string; signal: AbortSignal; acceptNavigation: () => boolean };

let nextRequestId = 1;
let request: PresentedFileRequest | null = null;
const listeners = new Set<() => void>();

export const presentedFileRequestSnapshot = () => request;
export const subscribePresentedFileRequest = (listener: () => void) => {
  listeners.add(listener);
  return () => listeners.delete(listener);
};

export function invalidateFileResourceNavigation(): void {
  cancelFileNavigation();
}

function publish(ref: FileResourceRef, action: PresentedFileRequest["action"], signal: AbortSignal) {
  if (signal.aborted) return;
  const layout = useLayoutStore.getState();
  let dockTabId: string;
  if (ref.hostId !== "local") {
    const remote = useRemoteStore.getState();
    remote.openExplorer(ref.hostId);
    remote.setExplorerTab("files");
    layout.setRightDockMode("remote");
    layout.setWorkspacePanelOpen(true);
    dockTabId = useActivityBarStore.getState().openEntry("remote", "Remote");
  } else {
    layout.setRightDockMode("files");
    layout.setWorkspacePanelOpen(true);
    dockTabId = useActivityBarStore.getState().openEntry("file", "Files");
  }
  let accepted = false;
  const acceptNavigation = () => {
    if (signal.aborted || accepted) return false;
    accepted = true;
    return true;
  };
  request = { id: nextRequestId++, ref, action, dockTabId, signal, acceptNavigation };
  signal.addEventListener("abort", () => {
    if (request?.signal !== signal) return;
    request = null;
    listeners.forEach(listener => listener());
  }, { once: true });
  listeners.forEach(listener => listener());
}

async function openBrowser(ref: FileResourceRef) {
  if (ref.hostId !== "local") throw new Error("Remote file browser preview is unavailable; save a copy to this device first");
  const signal = beginFileNavigation();
  const creation = ref.source === "presented"
    ? app.CreatePresentedBrowserPreviewForTab(ref.tabId, ref.toolCallId, ref.path)
    : app.CreateWorkspaceBrowserPreviewForTab(ref.tabId, ref.path);
  const url = await creation.catch(error => { if (!signal.aborted) throw error; return null; });
  if (!url) return;
  if (signal.aborted) {
    await app.RevokeWorkspaceBrowserPreview(url).catch(() => undefined);
    return;
  }
  const layout = useLayoutStore.getState();
  layout.setRightDockMode("browser");
  layout.setWorkspacePanelOpen(true);
  useActivityBarStore.getState().openEntry("browser", "Browser");
  try {
    await waitForBrowserHost();
    if (signal.aborted) {
      await app.RevokeWorkspaceBrowserPreview(url).catch(() => undefined);
      return;
    }
    await useBrowserPanelStore.getState().open(url, true, signal);
    if (signal.aborted) await app.RevokeWorkspaceBrowserPreview(url).catch(() => undefined);
  } catch (error) {
    await app.RevokeWorkspaceBrowserPreview(url).catch(() => undefined);
    if (!signal.aborted) throw error;
  }
}

export async function resolveFileResourcePath(ref: FileResourceRef): Promise<string> {
  if (ref.hostId !== "local") {
    return ref.source === "presented"
      ? app.ResolveRemotePresentedPathForTab(ref.tabId, ref.hostId, ref.toolCallId, ref.path)
      : app.ResolveRemoteWorkspacePathForTab(ref.tabId, ref.hostId, ref.toolCallId, ref.path);
  }
  return ref.source === "presented"
    ? app.ResolvePresentedPathForTab(ref.tabId, ref.toolCallId, ref.path)
    : app.ResolveWorkspacePathForTab(ref.tabId, ref.path);
}

export function openResource(ref: FileResourceRef, options: { view: "preview" | "source" | "browser" }): Promise<void> {
  return performResourceAction(ref, options.view);
}

export async function performResourceAction(ref: FileResourceRef, action: PresentedFileAction): Promise<void> {
  switch (action) {
    case "preview":
    case "reveal-tree":
    case "source": {
      const signal = beginFileNavigation();
      try {
        const target = ref.hostId !== "local" && ref.source === "workspace"
          ? { ...ref, path: await resolveFileResourcePath(ref) }
          : ref;
        publish(target, action, signal);
      } catch (error) { if (!signal.aborted) throw error; }
      return;
    }
    case "browser":
      return openBrowser(ref);
    case "open-native":
      cancelFileNavigation();
      if (ref.hostId !== "local") throw new Error("This remote host does not expose a desktop opener");
      return ref.source === "presented"
        ? app.OpenPresentedPathForTab(ref.tabId, ref.toolCallId, ref.path)
        : app.OpenWorkspacePathForTab(ref.tabId, ref.path);
    case "reveal-native":
      cancelFileNavigation();
      if (ref.hostId !== "local") throw new Error("This remote host does not expose a desktop file manager");
      return ref.source === "presented"
        ? app.RevealPresentedPathForTab(ref.tabId, ref.toolCallId, ref.path)
        : app.RevealWorkspacePathForTab(ref.tabId, ref.path);
    case "save-copy":
      cancelFileNavigation();
      if (ref.hostId !== "local") {
        if (ref.source === "presented") await app.SaveRemotePresentedFileAs(ref.tabId, ref.hostId, ref.toolCallId, ref.path);
        else await app.SaveRemoteFileAs(ref.hostId, await resolveFileResourcePath(ref));
      } else if (ref.source === "presented") await app.SavePresentedPathAsForTab(ref.tabId, ref.toolCallId, ref.path);
      else await app.SaveWorkspacePathAsForTab(ref.tabId, ref.path);
  }
}

/** Compatibility seam for callers created with the first present-file UI. */
export const performPresentedFileAction = performResourceAction;
