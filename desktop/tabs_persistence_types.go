package main

type desktopTabEntry struct {
	ID                string  `json:"id"`
	Scope             string  `json:"scope"`
	WorkspaceRoot     string  `json:"workspaceRoot"`
	TopicID           string  `json:"topicId"`
	SessionPath       string  `json:"sessionPath,omitempty"`
	ReadOnly          bool    `json:"readOnly,omitempty"`
	TakeoverSpectator bool    `json:"takeoverSpectator,omitempty"`
	Model             string  `json:"model,omitempty"`
	Effort            *string `json:"effort,omitempty"`
	TokenMode         string  `json:"tokenMode,omitempty"`
	AgentPreset       string  `json:"agentPreset,omitempty"`
	QualityFloor      string  `json:"qualityFloor,omitempty"`
	Mode              string  `json:"mode,omitempty"`
	Goal              string  `json:"goal,omitempty"`
	ToolApprovalMode  string  `json:"toolApprovalMode,omitempty"`
}

type desktopTabsFile struct {
	Tabs           []desktopTabEntry       `json:"tabs"`
	ActiveTab      string                  `json:"activeTab"`
	RemoteTabs     []desktopRemoteTabEntry `json:"remoteTabs,omitempty"`
	RemoteTabOrder []string                `json:"remoteTabOrder,omitempty"`
	TabOrder       []string                `json:"tabOrder,omitempty"`
}
