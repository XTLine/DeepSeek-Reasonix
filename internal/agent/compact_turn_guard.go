package agent

func (a *Agent) sameTurnCompactionBlocked(activeTurn int64, trigger string, mustFree bool) bool {
	if activeTurn == 0 || trigger == CompactionTriggerManual {
		return false
	}
	if a.sess.compaction.lastTurn.Load() != activeTurn {
		return false
	}
	return !mustFree || a.sess.compaction.recoveryTurn.Load() == activeTurn
}

func (a *Agent) noteMustFreeCompaction(activeTurn int64, trigger string, mustFree bool) {
	if activeTurn != 0 && trigger != CompactionTriggerManual && mustFree {
		a.sess.compaction.recoveryTurn.Store(activeTurn)
	}
}
