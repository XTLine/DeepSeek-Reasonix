/**
 * Session-scoped cache of measured transcript row heights.
 *
 * Virtuoso applies `heightEstimates` only while its size tree is empty, so
 * every full remount collapses the tree back to the static priors — and any
 * estimate-based scrollToIndex landing after the remount inherits the
 * prior's error, which grows with transcript length (#8657). This store
 * records the real heights returned by Virtuoso's `itemSize` callback and
 * synthesizes estimates from them:
 *
 *   measured height for this rowKey
 *   → median of recently measured rows of the same kind
 *   → static EAW-aware prior
 *
 * A remount that must happen anyway (surface switch) then restarts from
 * measured geometry instead of guesses, so the collapse mechanism is
 * disarmed even when a remount cannot be avoided.
 */

import { estimateTranscriptRowSize, transcriptRowMeasurementVersion, type TranscriptRow } from "./transcriptRows";

/** Samples kept per row kind for the median fallback. */
const KIND_SAMPLE_CAP = 100;

export type TranscriptMeasuredSizes = {
  /** Record one measured row height (px). Non-positive/NaN ignored. */
  record: (
    rowKey: string,
    kind: TranscriptRow["kind"],
    height: number,
    width?: number,
    measurementVersion?: string,
  ) => void;
  /** Estimate array aligned with `rows`, for Virtuoso's heightEstimates. */
  synthesize: (rows: readonly TranscriptRow[], width?: number) => number[];
  /** Drop all measurements (surface switch). */
  clear: () => void;
};

function medianOf(samples: readonly number[]): number | undefined {
  if (samples.length === 0) return undefined;
  const sorted = [...samples].sort((a, b) => a - b);
  const middle = sorted.length >> 1;
  return sorted.length % 2 === 1
    ? sorted[middle]
    : (sorted[middle - 1] + sorted[middle]) / 2;
}

export function createTranscriptMeasuredSizes(): TranscriptMeasuredSizes {
  type Sample = { kind: TranscriptRow["kind"]; height: number; width?: number; measurementVersion: string };
  const measured = new Map<string, Sample>();

  const widthMatches = (sample: Sample, width?: number) => width === undefined
    || (sample.width !== undefined && Math.abs(sample.width - width) <= 1);

  const record: TranscriptMeasuredSizes["record"] = (rowKey, kind, height, width, measurementVersion = "0:0") => {
    if (!Number.isFinite(height) || height <= 0) return;
    measured.delete(rowKey);
    measured.set(rowKey, {
      kind,
      height,
      width: Number.isFinite(width) && (width ?? 0) > 0 ? width : undefined,
      measurementVersion,
    });
  };

  const kindMedian = (kind: TranscriptRow["kind"], width?: number) => medianOf(
    [...measured.values()]
      .filter((sample) => sample.kind === kind && widthMatches(sample, width))
      .slice(-KIND_SAMPLE_CAP)
      .map((sample) => sample.height),
  );

  return {
    record,
    synthesize: (rows, width) => {
      // Remove changed-content samples before computing any kind median; a
      // stale exact height must not influence another row's fallback.
      for (const row of rows) {
        const rowKey = String(row.key);
        const sample = measured.get(rowKey);
        if (sample && sample.measurementVersion !== transcriptRowMeasurementVersion(row)) measured.delete(rowKey);
      }
      const medians = new Map<TranscriptRow["kind"], number | undefined>();
      return rows.map((row) => {
        const exact = measured.get(String(row.key));
        if (exact && widthMatches(exact, width)) return exact.height;
        if (!medians.has(row.kind)) medians.set(row.kind, kindMedian(row.kind, width));
        return medians.get(row.kind) ?? estimateTranscriptRowSize(row);
      });
    },
    clear: () => measured.clear(),
  };
}
