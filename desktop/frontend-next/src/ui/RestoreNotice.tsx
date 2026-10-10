import "./RestoreNotice.css";
import { t } from "../i18n";
import type { RestoreNotice as Receipt } from "./rewind";

export function RestoreNotice({ receipt, onUndo, onDismiss }: {
  receipt: Receipt;
  onUndo: (tx: string) => Promise<void>;
  onDismiss: () => void;
}) {
  return <div className="restore-notice" role="status">
    <span>{t("已还原 {n} 个文件", { n: receipt.files })}</span>
    <button data-action="rewind.undo" disabled={receipt.working} onClick={() => void onUndo(receipt.tx)}>{t("撤销这次还原")}</button>
    <button data-action="layer.dismiss" disabled={receipt.working} onClick={onDismiss}>{t("知道了")}</button>
    {receipt.error && <span role="alert">{receipt.error}</span>}
  </div>;
}
