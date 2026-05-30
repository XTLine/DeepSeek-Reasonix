import type { CommandInfo } from "../lib/types";

const KIND_TAG: Record<CommandInfo["kind"], string> = {
  builtin: "",
  custom: "project",
  mcp: "mcp",
};

// SlashMenu is the "/" autocomplete dropdown above the composer. Presentational:
// the Composer owns filtering, the active index, and key handling; this renders
// the list and reports hover/pick. Uses mousedown (not click) so picking an item
// doesn't blur the textarea first.
export function SlashMenu({
  items,
  activeIndex,
  onPick,
  onHover,
}: {
  items: CommandInfo[];
  activeIndex: number;
  onPick: (c: CommandInfo) => void;
  onHover: (i: number) => void;
}) {
  return (
    <div className="slashmenu" role="listbox">
      {items.map((c, i) => (
        <button
          key={c.kind + ":" + c.name}
          role="option"
          aria-selected={i === activeIndex}
          className={`slashmenu__item ${i === activeIndex ? "slashmenu__item--active" : ""}`}
          onMouseDown={(e) => {
            e.preventDefault();
            onPick(c);
          }}
          onMouseMove={() => onHover(i)}
        >
          <span className="slashmenu__name">/{c.name}</span>
          {c.hint && <span className="slashmenu__hint">{c.hint}</span>}
          <span className="slashmenu__desc">{c.description}</span>
          {KIND_TAG[c.kind] && <span className="slashmenu__kind">{KIND_TAG[c.kind]}</span>}
        </button>
      ))}
    </div>
  );
}
