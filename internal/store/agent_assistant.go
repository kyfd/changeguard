package store

import (
	"sort"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

// AgentConversations returns all conversations for a change, newest first.
func (s *Store) AgentConversations(organizationID, changeID string) []model.AgentConversation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.AgentConversation, 0, len(s.data.AgentConversations))
	for _, conversation := range s.data.AgentConversations {
		if conversation.OrganizationID == organizationID && conversation.ChangeID == changeID {
			items = append(items, conversation)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	return items
}

// AgentMessages returns messages for a conversation, oldest first.
func (s *Store) AgentMessages(organizationID, conversationID string) []model.AgentMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.AgentMessage, 0, len(s.data.AgentMessages))
	for _, message := range s.data.AgentMessages {
		if message.OrganizationID == organizationID && message.ConversationID == conversationID {
			items = append(items, message)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	return items
}

// CreateAgentConversation persists a new conversation with its first message.
func (s *Store) CreateAgentConversation(conversation model.AgentConversation, firstMessage model.AgentMessage, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.AgentConversations = append(s.data.AgentConversations, conversation)
	s.data.AgentMessages = append(s.data.AgentMessages, firstMessage)
	s.appendAuditsLocked(audit)
	return s.saveLocked()
}

// AppendAgentMessage persists an assistant message into an existing conversation.
func (s *Store) AppendAgentMessage(organizationID, conversationID string, message model.AgentMessage, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for index := range s.data.AgentConversations {
		if s.data.AgentConversations[index].OrganizationID == organizationID && s.data.AgentConversations[index].ID == conversationID {
			s.data.AgentConversations[index].UpdatedAt = time.Now()
			found = true
			break
		}
	}
	if !found {
		return ErrNotFound
	}
	// Only the assistant message is appended here; the user question was
	// persisted together with the assistant answer as one turn.
	s.data.AgentMessages = append(s.data.AgentMessages, message)
	s.appendAuditsLocked(audit)
	return s.saveLocked()
}
