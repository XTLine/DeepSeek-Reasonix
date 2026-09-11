import { partitionTurnItems } from "../lib/transcriptRows";
import assert from "node:assert/strict";
import React from "react";
import { LocaleProvider } from "../lib/i18n";
import { renderToStaticMarkup } from "react-dom/server";
import { historyMessagesToItems, initialState, reducer } from "../lib/useController";
import { NoticeCard } from "../components/TranscriptCards";
import { useTranscriptRowRenderer } from "../components/useTranscriptRowRenderer";
import { searchOutputMetadata, parseSearchSources } from "../lib/searchSources";
import type { WireEvent } from "../lib/types";
const ev=(s:typeof initialState,e:WireEvent)=>reducer(s,{type:"event",e});
let state=ev(initialState,{kind:"turn_started",turnId:"one"});
state=ev(state,{kind:"turn_done",turnId:"one",err:"opaque rejection",protocolRecovery:{id:"token"}});
const action=state.items.find(i=>i.kind==="notice"&&i.action==="recover_context");
assert(action?.kind==="notice");
assert.equal(action.recoveryId,"token");
const submissions: Array<{ display: string; submit?: string }> = [];
function RecoveryRow() {
  const render = useTranscriptRowRenderer({ checkpoints: [], subcallsByParent: new Map(), creationMode: false, running: false,
    actionPending: false, rewindDisabled: false, actionHoverMenus: false, lastTurn: 0,
    onFoldToggle: () => {}, onReasoningManualOpen: () => {}, onPrompt: (display, submit) => { submissions.push({ display, submit }); } });
  const row = render({ kind: "notice", key: "recovery", item: action as Extract<typeof action, { kind: "notice" }> });
  if (React.isValidElement<{ onAction?: () => void }>(row)) row.props.onAction?.();
  return row;
}
assert.match(renderToStaticMarkup(<LocaleProvider><RecoveryRow /></LocaleProvider>), /<button/, "the production row renderer exposes recovery");
assert.equal(submissions[0]?.submit, "/recover-context token");
assert(!submissions[0]?.display.includes("token"), "opaque recovery token is not user-facing copy");
assert.match(renderToStaticMarkup(<LocaleProvider><NoticeCard item={action} onAction={()=>{}} /></LocaleProvider>),/button/);
state=ev(state,{kind:"turn_started",turnId:"two"});
const before=state;
state=ev(state,{kind:"turn_done",turnId:"one",protocolRecovery:{id:"stale"}});
assert.equal(state,before);
state=ev(state,{kind:"turn_done",turnId:"two",status:"interrupted",protocolRecovery:{id:"cancelled"}});
assert(!state.items.some(i=>i.kind==="notice"&&i.recoveryId==="cancelled"));
const history=historyMessagesToItems([{role:"notice",content:"",code:"protocol_recovery",pending:true,protocolRecovery:{id:"saved"}},{role:"assistant",content:"Summary",serverSearch:[{id:"search",sources_status:"not_provided",results:[]}]}],"h").items;
assert(history.some(i=>i.kind==="notice"&&i.recoveryId==="saved"));
assert(history.some(i=>i.kind==="tool"&&i.searchSourcesStatus==="not_provided"&&i.status==="done"));
const output=JSON.stringify({sources_status:"not_provided",sources:[],summary:"https://unverified.invalid prose"});
assert.deepEqual(parseSearchSources(output),[]);
assert.deepEqual(searchOutputMetadata(output),{status:"not_provided",summary:"https://unverified.invalid prose"});
assert.deepEqual(searchOutputMetadata("old text"),{});
console.log("protocol recovery and search presentation passed");

assert.equal(partitionTurnItems([action])[0]?.outsideItems[0],action);
