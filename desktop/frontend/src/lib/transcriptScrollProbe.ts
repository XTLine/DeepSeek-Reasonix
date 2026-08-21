/**
 * Observability probe for every imperative scroll write against the
 * transcript Virtuoso handle. The single-writer contract (only the scroll
 * arbiter may call `virtuosoRef.current.scroll*`) is enforced statically by
 * scripts/check-single-scroll-writer.mjs; this probe is the runtime mirror:
 * tests and diagnostics can observe who wrote, what kind of write, and where
 * it landed, without intercepting the DOM.
 */
export type TranscriptScrollWriteRecord = {
  /** Logical writer, e.g. "tail-follow", "jump", "recovery", or a
   *  TranscriptScrollOwner such as "selection-edge-scroll". */
  owner: string;
  kind: "scrollTo" | "scrollBy" | "scrollToIndex";
  top?: number;
  index?: number | "LAST";
  source?: string;
  phase?: "initial" | "settle";
  scrollTop?: number;
  scrollHeight?: number;
  clientHeight?: number;
  bottomDistance?: number;
  mode?: string;
  settleFrame?: number;
  offBottomFrames?: number;
  stagnantFrames?: number;
};

type DiagnosticSink = (type: string, fields: Record<string, unknown>) => void;
let diagnosticSink: DiagnosticSink | undefined;
const CAPTURE_SCROLL_DIAGNOSTIC_DETAILS = typeof __BUILD_CHANNEL__ === "undefined"
  || __BUILD_CHANNEL__ === "test"
  || import.meta.env.DEV;

export function isTranscriptScrollDiagnosticsBuild(channel: string, development: boolean): boolean {
  return channel === "test" || development;
}

export function setTranscriptScrollDiagnosticSink(sink: DiagnosticSink): void {
  diagnosticSink = sink;
}

export function recordTranscriptScrollDiagnostic(type: string, fields: Record<string, unknown> = {}): void {
  diagnosticSink?.(type, fields);
}

declare global {
  interface Window {
    __REASONIX_TRANSCRIPT_SCROLL_WRITE__?: (write: TranscriptScrollWriteRecord) => void;
  }
}

export function noteTranscriptScrollWrite(write: TranscriptScrollWriteRecord): void {
  if (CAPTURE_SCROLL_DIAGNOSTIC_DETAILS) {
    recordTranscriptScrollDiagnostic("scroll-write", {
      owner: write.owner,
      writeKind: write.kind,
      targetTop: write.top,
      targetIndex: write.index,
      source: write.source,
      phase: write.phase,
      scrollTop: write.scrollTop,
      scrollHeight: write.scrollHeight,
      clientHeight: write.clientHeight,
      bottomDistance: write.bottomDistance,
      mode: write.mode,
      settleFrame: write.settleFrame,
      offBottomFrames: write.offBottomFrames,
      stagnantFrames: write.stagnantFrames,
    });
  }
  window.__REASONIX_TRANSCRIPT_SCROLL_WRITE__?.(write);
}

function finiteDatasetNumber(value: string | undefined): number | undefined {
  const parsed = Number.parseFloat(value ?? "");
  return Number.isFinite(parsed) ? parsed : undefined;
}

function rowFoldState(element: HTMLElement): { foldState: "none" | "open" | "closed" | "mixed"; disclosureCount: number } {
  const disclosures = Array.from(element.querySelectorAll<HTMLElement>("[aria-expanded]"));
  if (disclosures.length === 0) return { foldState: "none", disclosureCount: 0 };
  const states = new Set(disclosures.map((node) => node.getAttribute("aria-expanded") === "true"));
  return {
    foldState: states.size > 1 ? "mixed" : states.has(true) ? "open" : "closed",
    disclosureCount: disclosures.length,
  };
}

/** Records only geometry and fixed row classifications; text and row keys never leave the DOM. */
export function noteTranscriptRowMeasurement(element: HTMLElement, field: "offsetHeight" | "offsetWidth", measuredSize: number): void {
  if (field !== "offsetHeight") return;
  const previousSize = finiteDatasetNumber(element.dataset.knownSize);
  const estimatedSize = finiteDatasetNumber(element.dataset.estimatedSize);
  const comparisonSize = previousSize ?? estimatedSize;
  if (comparisonSize !== undefined && Math.abs(measuredSize - comparisonSize) <= 0.5) return;
  const rowIndex = finiteDatasetNumber(element.dataset.logicalIndex) ?? finiteDatasetNumber(element.dataset.index);
  const contentRevision = finiteDatasetNumber(element.dataset.contentRevision);
  const { foldState, disclosureCount } = rowFoldState(element);
  recordTranscriptScrollDiagnostic("row-measure", {
    rowIndex,
    rowKind: element.dataset.rowKind,
    estimatedSize,
    previousSize,
    measuredSize,
    sizeDelta: comparisonSize === undefined ? undefined : measuredSize - comparisonSize,
    contentRevision,
    foldState,
    disclosureCount,
  });
}
