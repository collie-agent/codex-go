package database

import (
	"context"
	"encoding/json"
	"gorm.io/gorm"
	"time"
)

// ConversationRun is the read-only HTTP view; recovery payloads stay private.
type ConversationRun struct {
	RunID       string                 `json:"run_id"`
	ConvID      string                 `json:"conv_id"`
	Goal        string                 `json:"goal"`
	Status      string                 `json:"status"`
	Result      json.RawMessage        `json:"result"`
	Error       string                 `json:"error"`
	FailureType string                 `json:"failure_type"`
	Mode        string                 `json:"mode"`
	CreateAt    time.Time              `json:"create_at"`
	UpdateAt    time.Time              `json:"update_at"`
	ToolCalls   []ConversationToolCall `json:"tool_calls"`
}

type ConversationToolCall struct {
	ExecutionID   string `json:"execution_id"`
	CallID        string `json:"call_id"`
	Position      int    `json:"position"`
	Name          string `json:"name"`
	Arguments     string `json:"arguments"`
	Status        string `json:"status"`
	Result        string `json:"result"`
	Error         string `json:"error"`
	Batch         int    `json:"batch"`
	BatchPosition int    `json:"batch_position"`
}

func (s *RunStore) ConversationRuns(ctx context.Context, convID string) ([]ConversationRun, error) {
	runs := make([]ConversationRun, 0)
	// One transaction keeps conversation membership and tool history in one snapshot.
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var conv Conversation
		if err := tx.First(&conv, "conv_id = ?", convID).Error; err != nil {
			return err
		}
		var records []AgentRun
		if err := tx.Where("conv_id = ?", convID).Order("create_at ASC").Order("run_id ASC").Find(&records).Error; err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		ids := make([]string, len(records))
		indexes := make(map[string]int, len(records))
		for i, r := range records {
			ids[i] = r.RunID
			indexes[r.RunID] = i
			runs = append(runs, ConversationRun{RunID: r.RunID, ConvID: r.ConvID, Goal: r.Goal, Status: r.Status, Result: r.Result, Error: r.Error, FailureType: r.FailureType, Mode: r.Mode, CreateAt: r.CreateAt, UpdateAt: r.UpdateAt, ToolCalls: []ConversationToolCall{}})
		}
		// Query by root run ownership, including every delegated execution instance.
		var calls []ToolExecution
		if err := tx.Where("run_id IN ?", ids).Order("create_at ASC").Order("execution_id ASC").Order("position ASC").Find(&calls).Error; err != nil {
			return err
		}
		for _, c := range calls {
			i := indexes[c.RunID]
			runs[i].ToolCalls = append(runs[i].ToolCalls, ConversationToolCall{ExecutionID: c.ExecutionID, CallID: c.CallID, Position: c.Position, Name: c.Name, Arguments: c.Arguments, Status: c.Status, Result: c.Result, Error: c.Error, Batch: c.Batch, BatchPosition: c.BatchPosition})
		}
		return nil
	})
	return runs, err
}
