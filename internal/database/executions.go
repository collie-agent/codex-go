package database

import (
	"context"
	"encoding/json"
	state "github.com/collie-agent/codex-go/internal/context"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// ExecutionInstance 隔离普通运行的 ReAct 上下文；运行终态单独收尾。
type ExecutionInstance struct {
	ExecutionID string    `gorm:"column:execution_id;type:varchar(64);primaryKey"`
	RunID       string    `gorm:"column:run_id;type:varchar(64);index"`
	Result      []byte    `gorm:"column:result;type:json"`
	CreateAt    time.Time `gorm:"column:create_at;autoCreateTime"`
	UpdateAt    time.Time `gorm:"column:update_at;autoUpdateTime"`
}

func (ExecutionInstance) TableName() string { return "execution_instance" }

type ToolExecution struct {
	ExecutionID   string    `gorm:"column:execution_id;type:varchar(64);primaryKey"`
	CallID        string    `gorm:"column:call_id;type:varchar(255)"`
	RunID         string    `gorm:"column:run_id;type:varchar(64);index"`
	Position      int       `gorm:"column:position;primaryKey"`
	Name          string    `gorm:"column:name;type:varchar(255)"`
	Arguments     string    `gorm:"column:arguments;type:longtext"`
	Batch         int       `gorm:"column:batch"`
	BatchPosition int       `gorm:"column:batch_position"`
	Status        string    `gorm:"column:status;type:varchar(32)"`
	Result        string    `gorm:"column:result;type:longtext"`
	Error         string    `gorm:"column:error;type:longtext"`
	Payload       []byte    `gorm:"column:payload;type:json"`
	CreateAt      time.Time `gorm:"column:create_at;autoCreateTime"`
	UpdateAt      time.Time `gorm:"column:update_at;autoUpdateTime"`
}

func (ToolExecution) TableName() string { return "tool_execution" }

// saveExecutions commits the graph and every instance through the same transaction.
// Never publish a child tool registration only inside the scheduler's JSON blob.
func saveExecutions(tx *gorm.DB, durable *state.DurableState) error {
	if len(durable.MultiAgentCheckpoint) == 0 {
		return saveExecution(tx, durable)
	}
	var graph state.DelegationGraph[json.RawMessage, json.RawMessage]
	if err := json.Unmarshal(durable.MultiAgentCheckpoint, &graph); err != nil {
		return err
	}
	for _, execution := range graph.Executions() {
		// The root identity is authoritative; local delegation IDs are never run IDs.
		execution.RunId = durable.RunId
		if err := saveExecution(tx, execution); err != nil {
			return err
		}
	}
	return nil
}

func saveExecution(tx *gorm.DB, durable *state.DurableState) error {
	if durable.ExecutionID == "" {
		return nil
	}
	instance := ExecutionInstance{ExecutionID: durable.ExecutionID, RunID: durable.RunId, Result: durable.ExecutionResult}
	if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&instance).Error; err != nil {
		return err
	}
	for i, call := range durable.ToolExecutions {
		payload, err := json.Marshal(call)
		if err != nil {
			return err
		}
		record := ToolExecution{ExecutionID: durable.ExecutionID, RunID: durable.RunId, CallID: call.ToolCallId, Position: i, Payload: payload, Name: call.ToolName, Arguments: call.Arguments, Batch: call.Batch, BatchPosition: call.Position, Status: string(call.Status), Result: call.Result, Error: call.Error}
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&record).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *RunStore) Execution(ctx context.Context, runID string) (*state.DurableState, error) {
	return s.ExecutionByID(ctx, runID, runID)
}

// ExecutionByID loads the history belonging to one execution instance.
func (s *RunStore) ExecutionByID(ctx context.Context, runID, executionID string) (*state.DurableState, error) {
	defer s.lockConnection()()
	var instance ExecutionInstance
	if err := s.db.WithContext(ctx).Where("run_id = ? AND execution_id = ?", runID, executionID).First(&instance).Error; err != nil {
		return nil, err
	}
	var calls []ToolExecution
	if err := s.db.WithContext(ctx).Where("execution_id = ?", instance.ExecutionID).Order("position ASC").Find(&calls).Error; err != nil {
		return nil, err
	}
	durable := &state.DurableState{RunId: runID, ExecutionID: instance.ExecutionID, ExecutionResult: instance.Result}
	for _, call := range calls {
		var result state.ToolResult
		if err := json.Unmarshal(call.Payload, &result); err != nil {
			return nil, err
		}
		durable.ToolExecutions = append(durable.ToolExecutions, &result)
	}
	return durable, nil
}
