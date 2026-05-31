/** Modal picker for `ask_choice` — options + optional "type my own" escape hatch. */

import { Box, Text } from "ink";
import React, { useRef, useState } from "react";
import { t } from "../../i18n/index.js";
import type { ChoiceOption } from "../../tools/choice.js";
import { SingleSelect } from "./Select.js";
import { ApprovalCard } from "./cards/ApprovalCard.js";
import { useKeystroke } from "./keystroke-context.js";
import { CARD, FG } from "./theme/tokens.js";
import { useTick } from "./ticker.js";

export type ChoiceConfirmChoice =
  | { kind: "pick"; optionId: string }
  | { kind: "custom"; text?: string }
  | { kind: "cancel" };

export interface ChoiceConfirmProps {
  question: string;
  options: ChoiceOption[];
  allowCustom: boolean;
  onChoose: (choice: ChoiceConfirmChoice) => void;
}

const CANCEL_VALUE = "__cancel__";
const CUSTOM_VALUE = "__custom__";

function ChoiceConfirmInner({ question, options, allowCustom, onChoose }: ChoiceConfirmProps) {
  const [customValue, setCustomValue] = useState("");
  const customValueRef = useRef("");
  const tick = useTick();
  const cursorOn = Math.floor(tick / 4) % 2 === 0;
  const items: Array<{ value: string; label: string; hint?: string }> = options.map((opt) => ({
    value: opt.id,
    label: `${opt.id} · ${opt.title}`,
    hint: opt.summary,
  }));
  if (allowCustom) {
    items.push({
      value: CUSTOM_VALUE,
      label: t("choiceConfirm.customLabel"),
      hint: t("choiceConfirm.customDesc"),
    });
  }
  items.push({
    value: CANCEL_VALUE,
    label: t("choiceConfirm.cancelLabel"),
    hint: t("choiceConfirm.cancelDesc"),
  });

  useKeystroke((ev) => {
    if (!allowCustom) return;
    if (
      ev.upArrow ||
      ev.downArrow ||
      ev.leftArrow ||
      ev.rightArrow ||
      ev.return ||
      ev.escape ||
      ev.tab ||
      ev.pageUp ||
      ev.pageDown
    ) {
      return;
    }
    if (ev.paste) {
      const next = customValueRef.current + ev.input.replace(/\r?\n/g, " ");
      customValueRef.current = next;
      setCustomValue(next);
      return;
    }
    if ((ev.backspace || ev.delete) && customValueRef.current.length > 0) {
      const next = customValueRef.current.slice(0, -1);
      customValueRef.current = next;
      setCustomValue(next);
      return;
    }
    if (ev.input && !ev.ctrl && !ev.meta) {
      const next = customValueRef.current + ev.input;
      customValueRef.current = next;
      setCustomValue(next);
    }
  });

  return (
    <ApprovalCard tone="info" title={question} metaRight={t("shellConfirm.awaiting")}>
      <SingleSelect
        initialValue={options[0]?.id}
        items={items}
        onSubmit={(v) => {
          if (v === CUSTOM_VALUE) onChoose({ kind: "custom", text: customValueRef.current.trim() });
          else if (v === CANCEL_VALUE) onChoose({ kind: "cancel" });
          else onChoose({ kind: "pick", optionId: v });
        }}
        onCancel={() => onChoose({ kind: "cancel" })}
      />
      {allowCustom ? (
        <Box flexDirection="column" marginTop={1}>
          <Text color={FG.sub}>{t("planFlow.modes.choice-custom.hint")}</Text>
          <Box>
            <Text color={CARD.plan.color} bold>
              {"› "}
            </Text>
            <Text>{customValue}</Text>
            <Text color={CARD.plan.color} bold>
              {cursorOn ? "▍" : " "}
            </Text>
          </Box>
        </Box>
      ) : null}
    </ApprovalCard>
  );
}

export const ChoiceConfirm = React.memo(ChoiceConfirmInner);
