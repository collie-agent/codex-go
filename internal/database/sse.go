package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/collie-agent/codex-go/internal/entity"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SSEStreamEvent is the GORM representation of a persisted SSE event.
type SSEStreamEvent struct {
	ID          uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID      string    `gorm:"column:task_id;size:36;not null;uniqueIndex:uk_sse_stream_events_task_event,priority:1;index:idx_sse_stream_events_task_created,priority:1"`
	LastEventID string    `gorm:"column:last_event_id;size:64;not null;uniqueIndex:uk_sse_stream_events_task_event,priority:2"`
	Data        string    `gorm:"column:data;type:mediumtext;not null"`
	CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime:milli;index:idx_sse_stream_events_task_created,priority:2"`
}

// TableName keeps GORM aligned with scripts/create_sse_stream_events.sql.
func (SSEStreamEvent) TableName() string { return "sse_stream_events" }

// SSEEventStore persists response stream batches using the shared database connection.
type SSEEventStore struct{ DB *gorm.DB }

func NewSSEEventStore(db *gorm.DB) *SSEEventStore { return &SSEEventStore{DB: db} }

func (m *SSEEventStore) WriteSSEEvent(ctx context.Context, taskID string, events []*entity.StreamEvent) error {
	if m == nil || m.DB == nil {
		return errors.New("db: manager is not configured")
	}
	if taskID == "" {
		return errors.New("db: task ID is empty")
	}
	if len(events) == 0 {
		return nil
	}

	eventStr, _ := json.Marshal(events)
	record := SSEStreamEvent{
		TaskID:      taskID,
		LastEventID: events[len(events)-1].ID,
		Data:        string(eventStr),
	}
	result := m.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "task_id"}, {Name: "last_event_id"}},
		DoNothing: true,
	}).Create(&record)
	if result.Error != nil {
		return fmt.Errorf("write SSE event: %w", result.Error)
	}
	return nil
}

func (m *SSEEventStore) ReadSSEEvents(ctx context.Context, taskID string) ([]*entity.StreamEvent, string, error) {
	if m == nil || m.DB == nil {
		return nil, "", errors.New("db: manager is not configured")
	}
	if taskID == "" {
		return nil, "", errors.New("db: task ID is empty")
	}

	query := m.DB.WithContext(ctx).Where("task_id = ?", taskID)

	var records []SSEStreamEvent
	if err := query.Order("id ASC").Find(&records).Error; err != nil {
		return nil, "", fmt.Errorf("read SSE events: %w", err)
	}

	var lastEventId string
	events := make([]*entity.StreamEvent, 0, len(records))
	for _, record := range records {
		eventDatas := make([]*entity.StreamEvent, 0)
		if err := json.Unmarshal([]byte(record.Data), &eventDatas); err != nil {
			return nil, "", err
		}
		for _, event := range eventDatas {
			events = append(events, event)
		}
		lastEventId = record.LastEventID
	}
	return events, lastEventId, nil
}
