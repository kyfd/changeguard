package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

// AskChangeAssistantInput is the payload for a change-scoped Evidence Navigator question.
type AskChangeAssistantInput struct {
	Question       string `json:"question"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// AgentConversationSummary is the public view of a conversation for the API.
type AgentConversationSummary struct {
	Conversation model.AgentConversation `json:"conversation"`
	Messages     []model.AgentMessage    `json:"messages"`
}

// AskChangeAssistant answers a question about a specific change. It is
// read-only: it can explain status, cite evidence and propose actions, but it
// never mutates governance state. Every answer is bound to the change, the
// organization and the requesting actor.
func (s *Service) AskChangeAssistant(ctx context.Context, changeID, actorID string, input AskChangeAssistantInput) (model.AgentMessage, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.AgentMessage{}, err
	}
	change, err := s.ChangeFor(changeID, actorID)
	if err != nil {
		return model.AgentMessage{}, err
	}
	question := strings.TrimSpace(input.Question)
	if question == "" {
		return model.AgentMessage{}, ErrValidation
	}
	if len([]rune(question)) > 2000 {
		return model.AgentMessage{}, fmt.Errorf("%w：问题过长", ErrValidation)
	}

	// The assistant answers purely from the deterministic evidence attached to
	// this change plus a small set of read-only tools. It cannot approve,
	// sign passports, deploy or mutate any governance state.
	answer := buildAssistantAnswer(ctx, change, s.evidenceTools, question)

	conversationID := strings.TrimSpace(input.ConversationID)
	conversation, existing, err := s.conversationFor(actor, change, conversationID)
	if err != nil {
		return model.AgentMessage{}, err
	}

	message := model.AgentMessage{
		ID:             store.NewID("msg_"),
		OrganizationID: actor.OrganizationID,
		ConversationID: conversation.ID,
		Role:           "assistant",
		Content:        answer.Answer,
		Question:       question,
		Intent:         answer.Intent,
		Answer:         answer.Answer,
		Citations:      answer.Citations,
		Trace:          answer.Trace,
		Proposals:      answer.Proposals,
		CreatedAt:      time.Now(),
	}
	if !existing {
		err = s.store.CreateAgentConversation(conversation, message, audit(actor, changeID, "AGENT_ASK_CREATE", "创建变更助手会话"))
	} else {
		err = s.store.AppendAgentMessage(actor.OrganizationID, conversation.ID, message, audit(actor, changeID, "AGENT_ASK", "变更助手回答："+truncate(question, 40)))
	}
	if err != nil {
		return model.AgentMessage{}, err
	}
	return message, nil
}

// AgentConversationFor returns one conversation (with messages) if the actor is
// allowed to see it. Conversations are change-scoped, so ChangeFor already
// enforces tenant and application access.
func (s *Service) AgentConversationFor(changeID, conversationID, actorID string) (AgentConversationSummary, error) {
	if _, err := s.ChangeFor(changeID, actorID); err != nil {
		return AgentConversationSummary{}, err
	}
	actor, err := s.activeActor(actorID)
	if err != nil {
		return AgentConversationSummary{}, err
	}
	conversations := s.store.AgentConversations(actor.OrganizationID, changeID)
	for _, conversation := range conversations {
		if conversation.ID == conversationID {
			return AgentConversationSummary{
				Conversation: conversation,
				Messages:     s.store.AgentMessages(actor.OrganizationID, conversationID),
			}, nil
		}
	}
	return AgentConversationSummary{}, store.ErrNotFound
}

// AgentConversationsForChange lists conversations for a change.
func (s *Service) AgentConversationsForChange(changeID, actorID string) ([]model.AgentConversation, error) {
	if _, err := s.ChangeFor(changeID, actorID); err != nil {
		return nil, err
	}
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, err
	}
	return s.store.AgentConversations(actor.OrganizationID, changeID), nil
}

func (s *Service) conversationFor(actor model.User, change model.ChangeRequest, conversationID string) (model.AgentConversation, bool, error) {
	if strings.TrimSpace(conversationID) != "" {
		conversations := s.store.AgentConversations(actor.OrganizationID, change.ID)
		for _, conversation := range conversations {
			if conversation.ID == conversationID {
				return conversation, true, nil
			}
		}
		return model.AgentConversation{}, false, store.ErrNotFound
	}
	// A new conversation is created per change; one active conversation per
	// change and creator is enough to keep the UI simple.
	for _, conversation := range s.store.AgentConversations(actor.OrganizationID, change.ID) {
		if conversation.CreatorID == actor.ID {
			return conversation, true, nil
		}
	}
	conversation := model.AgentConversation{
		ID:             store.NewID("conv_"),
		OrganizationID: actor.OrganizationID,
		ChangeID:       change.ID,
		CreatorID:      actor.ID,
		CreatorName:    actor.Name,
		Title:          "变更助手：" + change.ID,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	return conversation, false, nil
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
