import { useI18n } from "../lib/i18n";
import type { GoalLifecycleView } from "../lib/types";

export function GoalLifecycleActions({
  goalView,
  goalStatus,
  disabled,
  running,
  onEditGoal,
  onPauseGoal,
  onResumeGoal,
  onStopGoal,
}: {
  goalView?: GoalLifecycleView;
  goalStatus?: string;
  disabled?: boolean;
  running: boolean;
  onEditGoal: (objective: string, maxGoalRounds: number | null) => void;
  onPauseGoal: () => void;
  onResumeGoal: () => void;
  onStopGoal: () => void;
}) {
  const { t } = useI18n();
  const resumable = goalView?.phase === "paused"
    || goalView?.phase === "blocked"
    || (goalView?.phase === "active" && goalView.activation === "disarmed")
    || (!goalView && goalStatus === "blocked");

  const editGoal = () => {
    if (!goalView) return;
    const objective = window.prompt(t("composer.goalEditObjective"), goalView.objective);
    if (objective === null) return;
    const rawLimit = window.prompt(t("composer.goalEditMaxRounds"), goalView.maxGoalRounds?.toString() ?? "");
    if (rawLimit === null) return;
    const trimmedLimit = rawLimit.trim();
    const parsedLimit = trimmedLimit === "" ? null : Number(trimmedLimit);
    if (parsedLimit !== null && (!Number.isSafeInteger(parsedLimit) || parsedLimit <= 0)) {
      window.alert(t("composer.goalEditInvalidRounds"));
      return;
    }
    onEditGoal(objective, parsedLimit);
  };

  return <>
    {goalView && <button type="button" className="composer-intent-menu__stop" onClick={editGoal} disabled={disabled}>
      {t("composer.taskModeEditGoal")}
    </button>}
    {resumable ? (
      <button type="button" className="composer-intent-menu__stop" onClick={onResumeGoal} disabled={disabled}>
        {t("composer.taskModeResumeGoal")}
      </button>
    ) : (
      <button type="button" className="composer-intent-menu__stop" onClick={onPauseGoal} disabled={disabled}>
        {t("composer.taskModePauseGoal")}
      </button>
    )}
    <button type="button" className="composer-intent-menu__stop" onClick={onStopGoal} disabled={disabled || running}>
      {t("composer.taskModeStopGoal")}
    </button>
  </>;
}
