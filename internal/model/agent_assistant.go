package model

import "time"

// AgentConversation is a persisted change-scoped Clawbot conversation. Each
// conversation is bound to one organization, one change and one creator so the
// assistant can never leak context across tenants or change tickets.
type AgentConversation struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	ChangeID       string    `json:"change_id"`
	CreatorID      string    `json:"creator_id"`
	CreatorName    string    `json:"creator_name"`
	Title          string    `json:"title"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// AgentMessage is one turn inside a conversation: a user question and the
// assistant answer, always carrying the citations (evidence) the answer is
// based on. Answers may also propose actions, but the assistant itself never
// mutates governance state in v1.
type AgentMessage struct {
	ID             string                `json:"id"`
	OrganizationID string                `json:"organization_id"`
	ConversationID string                `json:"conversation_id"`
	Role           string                `json:"role"` // user | assistant
	Content        string                `json:"content"`
	Question       string                `json:"question,omitempty"`
	Answer         string                `json:"answer,omitempty"`
	Citations      []AgentCitation       `json:"citations"`
	Trace          []AgentToolTrace      `json:"trace"`
	Proposals      []AgentActionProposal `json:"proposals"`
	CreatedAt      time.Time             `json:"created_at"`
}

// AgentCitation is a single verifiable evidence reference attached to an answer.
type AgentCitation struct {
	Kind    string `json:"kind"` // rule_finding | experiment | change | policy | passport | history | topology | sql_scan | outcome
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	URL     string `json:"url,omitempty"`
}

// AgentToolTrace records which read-only tools were invoked while answering.
type AgentToolTrace struct {
	Tool     string `json:"tool"`
	Input    string `json:"input,omitempty"`
	Output   string `json:"output,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// AgentActionProposal is a suggested follow-up that requires explicit human
// confirmation. It is informational only; the assistant never executes it.
type AgentActionProposal struct {
	Type        string `json:"type"` // remediate | rerun_experiment | reassign | review | observe
	Title       string `json:"title"`
	Description string `json:"description"`
	ChangeID    string `json:"change_id,omitempty"`
	FindingID   string `json:"finding_id,omitempty"`
}

// ActionProposalType enumerates the read-only action kinds the assistant may
// propose. The assistant never executes these itself.
type ActionProposalType string

const (
	ProposalRemediate       ActionProposalType = "remediate"
	ProposalRerunExperiment ActionProposalType = "rerun_experiment"
	ProposalReassign        ActionProposalType = "reassign"
	ProposalReview          ActionProposalType = "review"
	ProposalObserve         ActionProposalType = "observe"
)
