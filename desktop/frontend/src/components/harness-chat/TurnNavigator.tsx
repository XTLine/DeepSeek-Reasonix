// Ported from DeepSeek Harness c291e7961a (MIT). Only loaded Reasonix turns navigate.
import {
  memo, useEffect, useId, useRef, useState,
  type CSSProperties, type MouseEvent, type PointerEvent,
} from 'react'
import type { ReactNode } from 'react'
import type { useT } from '../../lib/i18n'
export interface TurnRailItem { turn: string; ordinal: number; prompt: string; response: string; answerKey?: string; anchor: { kind: 'loaded' } }
import css from './TurnNavigator.styles'

interface TurnNavigatorProps {
  readonly items: readonly TurnRailItem[]
  readonly activeTurn: string | null
  /** Turn whose jump is still paging history in; its mark pulses. */
  readonly busyTurn: string | null
  readonly onNavigate: (item: TurnRailItem) => void
  readonly renderPreview: (item: TurnRailItem) => ReactNode
  readonly t: ReturnType<typeof useT>
}

/** Fixed pitch between neighbouring marks; overflow scrolls inside the frame. */
const TURN_SPACING_PX = 10
/** Rail padding above the first mark and below the last one, per end. */
const RAIL_INSET_PX = 6
/** Fade band the mask reserves at a scrollable end. */
const FADE_PX = 24

type TurnPositionStyle = CSSProperties & {
  readonly '--turn-natural-position': string
}

type TurnFrameStyle = CSSProperties & {
  readonly '--turn-natural-height': string
  readonly '--turn-rail-inset': string
  readonly '--turn-scroll-top': string
}

function itemPosition(index: number): TurnPositionStyle {
  return { '--turn-natural-position': `${String(index * TURN_SPACING_PX)}px` }
}

function frameStyle(count: number, scrollTop: number): TurnFrameStyle {
  return {
    '--turn-natural-height': `${String((count - 1) * TURN_SPACING_PX + 2 * RAIL_INSET_PX)}px`,
    '--turn-rail-inset': `${String(RAIL_INSET_PX)}px`,
    '--turn-scroll-top': `${String(scrollTop)}px`,
  }
}

function itemAtPointer(
  items: readonly TurnRailItem[],
  frame: HTMLElement,
  scrollTop: number,
  clientY: number,
): TurnRailItem | undefined {
  const rect = frame.getBoundingClientRect()
  const offset = clientY - rect.top + scrollTop - RAIL_INSET_PX
  const index = Math.max(0, Math.min(items.length - 1, Math.round(offset / TURN_SPACING_PX)))
  return items[index]
}

/** Scroll state the mask fades and follow logic read together. */
interface RailScrollState {
  readonly top: number
  readonly viewportHeight: number
}

const RAIL_AT_REST: RailScrollState = { top: 0, viewportHeight: 0 }

function railScrollState(scroller: HTMLElement, viewportHeight: number): RailScrollState {
  return { top: scroller.scrollTop, viewportHeight }
}

function sameRailScrollState(left: RailScrollState, right: RailScrollState): boolean {
  return left.top === right.top
    && left.viewportHeight === right.viewportHeight
}

function TurnNavigatorRail({ items, activeTurn, busyTurn, onNavigate, renderPreview, t }: TurnNavigatorProps) {
  const [previewTurn, setPreviewTurn] = useState<string | null>(null)
  const [scrollState, setScrollState] = useState<RailScrollState>(RAIL_AT_REST)
  const scrollerRef = useRef<HTMLDivElement | null>(null)
  /** While the pointer works the rail, follow must not move it under the hand. */
  const pointerInsideRef = useRef(false)
  const previewId = useId()
  const hasItems = items.length > 1

  const syncScrollState = (viewportHeight?: number): void => {
    const scroller = scrollerRef.current
    if (scroller === null) return
    setScrollState(current => {
      const next = railScrollState(scroller, viewportHeight ?? current.viewportHeight)
      return sameRailScrollState(current, next) ? current : next
    })
  }

  // Frame resizes (band/composer changes) move the overflow edges without a
  // scroll event; item count changes move the content height the same way.
  useEffect(() => {
    const scroller = scrollerRef.current
    if (scroller === null || typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver(entries => { syncScrollState(entries[0]?.contentRect.height ?? 0) })
    observer.observe(scroller)
    return () => { observer.disconnect() }
  }, [hasItems])

  // Keep the active mark visible: centre it whenever it leaves the scrollport,
  // unless the reader's pointer is working the rail.
  useEffect(() => {
    const scroller = scrollerRef.current
    const index = items.findIndex(item => item.turn === activeTurn)
    if (scroller === null || index < 0 || pointerInsideRef.current) return
    const markTop = index * TURN_SPACING_PX + RAIL_INSET_PX
    const viewTop = scrollState.top
    const viewHeight = scrollState.viewportHeight
    if (viewHeight <= 0 || (markTop >= viewTop + FADE_PX && markTop <= viewTop + viewHeight - FADE_PX)) return
    const target = Math.max(0, markTop - viewHeight / 2)
    const reduced = typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches
    if (typeof scroller.scrollTo === 'function') {
      scroller.scrollTo({ top: target, behavior: reduced ? 'auto' : 'smooth' })
    } else {
      scroller.scroll?.({ top: target })
    }
  }, [activeTurn, items, scrollState.top, scrollState.viewportHeight])

  if (items.length < 2) return null
  const previewIndex = items.findIndex(item => item.turn === previewTurn)
  const preview = previewIndex < 0 ? undefined : items[previewIndex]
  const previewPosition = previewIndex < 0 ? undefined : itemPosition(previewIndex)
  const previewAtPointer = (event: PointerEvent<HTMLElement>): void => {
    const scrollTop = scrollerRef.current?.scrollTop ?? 0
    setPreviewTurn(itemAtPointer(items, event.currentTarget, scrollTop, event.clientY)?.turn ?? null)
  }
  const navigateAtPointer = (event: MouseEvent<HTMLElement>): void => {
    const scrollTop = scrollerRef.current?.scrollTop ?? 0
    const item = itemAtPointer(items, event.currentTarget, scrollTop, event.clientY)
    if (item !== undefined) onNavigate(item)
  }
  const fadeClasses = [css.scroller]
  const naturalHeight = (items.length - 1) * TURN_SPACING_PX + 2 * RAIL_INSET_PX
  const viewportHeight = scrollState.viewportHeight || Math.min(naturalHeight, 420)
  const firstVisible = Math.max(0, Math.floor((scrollState.top - RAIL_INSET_PX) / TURN_SPACING_PX) - 4)
  const lastVisible = Math.min(items.length, Math.ceil((scrollState.top + viewportHeight) / TURN_SPACING_PX) + 4)
  if (scrollState.top > 1) fadeClasses.push(css.fadeTop)
  if (scrollState.top < naturalHeight - viewportHeight - 1) fadeClasses.push(css.fadeBottom)
  return (
    <div className={css.slot}>
      <nav
        className={css.frame}
        style={frameStyle(items.length, scrollState.top)}
        aria-label={t('chat.turnNavigation.label')}
        onClick={navigateAtPointer}
        onPointerMove={previewAtPointer}
        onPointerEnter={() => { pointerInsideRef.current = true }}
        onPointerLeave={() => {
          pointerInsideRef.current = false
          setPreviewTurn(null)
        }}
      >
        <div
          ref={scrollerRef}
          className={fadeClasses.join(' ')}
          onScroll={() => { syncScrollState() }}
        >
          <div className={css.marks}>
            {items.slice(firstVisible, lastVisible).map((item, visibleIndex) => {
              const index = firstVisible + visibleIndex
              const active = item.turn === activeTurn
              const showingPreview = item.turn === previewTurn
              const previewDistance = previewIndex < 0 ? -1 : Math.abs(index - previewIndex)
              const classes = [css.mark]

              if (active) classes.push(css.markActive)
              else if (showingPreview) classes.push(css.markPreview)
              if (item.turn === busyTurn) classes.push(css.markBusy)
              return (
                <div key={item.turn} className={css.markPosition} style={itemPosition(index)}>
                  <button
                    data-nav-turn={item.turn}
                    type="button"
                    className={classes.join(' ')}
                    aria-label={t(
                      'chat.turnNavigation.jump',
                      { turn: item.ordinal },
                    )}
                    aria-current={active ? 'true' : undefined}
                    aria-busy={item.turn === busyTurn ? 'true' : undefined}
                    data-preview-distance={previewDistance >= 0 && previewDistance <= 2 ? previewDistance : undefined}
                    aria-describedby={showingPreview ? previewId : undefined}
                    onClick={(event) => {
                      event.stopPropagation()
                      onNavigate(item)
                    }}
                    onFocus={() => { setPreviewTurn(item.turn) }}
                    onBlur={() => { setPreviewTurn(null) }}
                  />
                </div>
              )
            })}
          </div>
        </div>
        {preview !== undefined && previewPosition !== undefined && (
          <div id={previewId} role="tooltip" className={css.preview} style={previewPosition}>
            {renderPreview(preview)}
          </div>
        )}
      </nav>
    </div>
  )
}

/**
 * Fixed-pitch rail of loaded turns with hover and focus previews. History is
 * loaded by the transcript's existing paging action. Overflow scrolls
 * inside the frame, gradient fades marking each scrollable end, and the
 * active mark keeps itself in view while the pointer is elsewhere.
 *
 * Memoized because it renders two host elements per Turn while the
 * enclosing view re-renders on every streaming delta: without the guard a long
 * session rebuilds hundreds of marks per commit for a rail that only changes
 * when a Turn is added, removed, or becomes active. Its props must therefore
 * stay referentially stable across those commits.
 */
export const TurnNavigator = memo(TurnNavigatorRail)
