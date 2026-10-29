package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	fileContext "github.com/collie-agent/codex-go/internal/context"
	"gorm.io/gorm"
)

// Conversation 只标识运行归属，不保存或共享模型消息。
type Conversation struct {
	ConvID   string    `gorm:"column:conv_id;type:varchar(64);primaryKey"`
	CreateAt time.Time `gorm:"column:create_at;autoCreateTime"`
	UpdateAt time.Time `gorm:"column:update_at;autoUpdateTime"`
}

func (Conversation) TableName() string { return "conversation" }

// AgentRun 只保存恢复所需的业务状态；不含凭据和 YAML 配置快照。
type AgentRun struct {
	RunID              string          `gorm:"column:run_id;type:varchar(64);primaryKey"`
	ConvID             string          `gorm:"column:conv_id;type:varchar(64);not null;index"`
	Goal               string          `gorm:"column:goal;type:longtext;not null"`
	Status             string          `gorm:"column:status;type:varchar(16);not null"`
	Result             json.RawMessage `gorm:"column:result;type:json"`
	Error              string          `gorm:"column:error;type:longtext"`
	Mode               string          `gorm:"column:mode;type:varchar(32)"`
	CompletionCriteria []string        `gorm:"column:completion_criteria;serializer:json;type:json"`
	PlanningContext    string          `gorm:"column:planning_context;type:longtext"`
	Workspace          string          `gorm:"column:workspace;type:longtext;not null"`
	CreateAt           time.Time       `gorm:"column:create_at;autoCreateTime"`
	UpdateAt           time.Time       `gorm:"column:update_at;autoUpdateTime"`
}

func (AgentRun) TableName() string { return "agent_run" }

// StepExecution stores one plan step.  JSON fields retain the structured
// evidence and observation emitted by the executor.
type StepExecution struct {
	StepID             string    `gorm:"column:step_id;type:varchar(128);not null;primaryKey"`
	RunID              string    `gorm:"column:run_id;type:varchar(64);not null;primaryKey;index"`
	Status             string    `gorm:"column:status;type:varchar(16);not null"`
	Intent             string    `gorm:"column:intent;type:longtext;not null"`
	CompletionCriteria string    `gorm:"column:completion_criteria;type:longtext;not null"`
	CompletionEvidence []string  `gorm:"column:completion_evidence;serializer:json;type:json"`
	Observation        []string  `gorm:"column:observation;serializer:json;type:json"`
	Position           int       `gorm:"column:position;not null"`
	Revision           int       `gorm:"column:revision;not null"`
	CreateAt           time.Time `gorm:"column:create_at;autoCreateTime"`
	UpdateAt           time.Time `gorm:"column:update_at;autoUpdateTime"`
}

func (StepExecution) TableName() string { return "step_execution" }

// Migrate 必须由独立命令显式调用；普通运行不会自动建表。
func Migrate(ctx context.Context, db *gorm.DB) error {
	db = db.WithContext(ctx)
	// 新库使用独立 SQL 建表，已有库仍通过 AutoMigrate 升级。
	schemaSQL, err := loadSchema()
	if err != nil {
		return err
	}
	for _, statement := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	if err := db.AutoMigrate(&Conversation{}, &AgentRun{}, &StepExecution{}, &SSEStreamEvent{}); err != nil {
		return err
	}
	// 显式指定外键方向，避免两个模型同名 ConvID 被推断为反向关联。
	if !db.Migrator().HasConstraint(&AgentRun{}, "fk_agent_run_conversation") {
		if err := db.Exec("ALTER TABLE agent_run ADD CONSTRAINT fk_agent_run_conversation FOREIGN KEY (conv_id) REFERENCES conversation(conv_id)").Error; err != nil {
			return err
		}
	}
	if !db.Migrator().HasConstraint(&StepExecution{}, "fk_step_execution_agent_run") {
		if err := db.Exec("ALTER TABLE step_execution ADD CONSTRAINT fk_step_execution_agent_run FOREIGN KEY (run_id) REFERENCES agent_run(run_id)").Error; err != nil {
			return err
		}
	}
	return nil
}

type RunStore struct {
	db *gorm.DB
	// 同一持锁连接的检查点事务与健康检查必须串行，防止查询插入事务中。
	mu *sync.Mutex
}

func (s *RunStore) lockConnection() func() {
	if s.mu == nil {
		return func() {}
	}
	s.mu.Lock()
	return s.mu.Unlock
}

func NewRunStore(db *gorm.DB) *RunStore { return &RunStore{db: db} }

// RunLock holds a dedicated MySQL connection for one run. Keeping the same
// connection for checkpoints is required because GET_LOCK is connection scoped.
type RunLock struct {
	conn     *sql.Conn
	name     string
	db       *gorm.DB
	released bool
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelCauseFunc
	done     chan struct{}
}

// AcquireRunLock obtains the run mutex without waiting. The connection is
// intentionally removed from the pool until Release, including on failures.
func (s *RunStore) AcquireRunLock(ctx context.Context, runID string) (*RunLock, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("run lock: database is required")
	}
	if runID == "" {
		return nil, fmt.Errorf("run lock: run_id is required")
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return nil, fmt.Errorf("run lock: get SQL connection: %w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("run lock: acquire connection: %w", err)
	}
	name := "codex:run:" + runID
	var acquired int
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("run lock: get_lock: %w", err)
	}
	if acquired != 1 {
		_ = conn.Close()
		return nil, fmt.Errorf("run %s is already running", runID)
	}
	// Context forces GORM to clone Statement; assigning ConnPool below must not
	// replace the shared pool on s.db, which future lock acquisitions need.
	lockedDB := s.db.Session(&gorm.Session{NewDB: true, Context: ctx})
	lockedDB.Statement.ConnPool = conn
	runCtx, cancel := context.WithCancelCause(ctx)
	lock := &RunLock{conn: conn, name: name, db: lockedDB, ctx: runCtx, cancel: cancel, done: make(chan struct{})}
	go lock.watch()
	return lock, nil
}

// Store returns a RunStore whose writes use the lock-holding connection.
func (l *RunLock) Store() *RunStore {
	if l == nil {
		return nil
	}
	return &RunStore{db: l.db, mu: &l.mu}
}

// Context is canceled when the caller cancels or the lock connection is lost.
// Models, MCP subprocesses, and tools must all use this context.
func (l *RunLock) Context() context.Context { return l.ctx }

// watch checks the dedicated connection even while a model or tool is running.
// This is connection health monitoring, not a lease or lock renewal.
func (l *RunLock) watch() {
	defer close(l.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			l.mu.Lock()
			checkCtx, cancel := context.WithTimeout(l.ctx, time.Second)
			var owned sql.NullBool
			err := l.conn.QueryRowContext(checkCtx, "SELECT IS_USED_LOCK(?) = CONNECTION_ID()", l.name).Scan(&owned)
			cancel()
			l.mu.Unlock()
			if l.ctx.Err() != nil {
				return
			}
			if err != nil {
				l.cancel(fmt.Errorf("run lock: connection lost: %w", err))
				return
			}
			if !owned.Valid || !owned.Bool {
				l.cancel(fmt.Errorf("run lock: ownership lost"))
				return
			}
		}
	}
}

// Release explicitly drops the MySQL lock and returns its connection to pool.
func (l *RunLock) Release(ctx context.Context) error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	l.cancel(nil)
	<-l.done
	// 运行 context 可能已取消；释放锁仍需独立的短超时，避免带锁连接回池。
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	var released sql.NullInt64
	err := l.conn.QueryRowContext(releaseCtx, "SELECT RELEASE_LOCK(?)", l.name).Scan(&released)
	if err != nil {
		// RELEASE_LOCK 失败时丢弃物理连接，由 MySQL 释放连接锁。
		_ = l.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	closeErr := l.conn.Close()
	if err != nil {
		return fmt.Errorf("run lock: release: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("run lock: close connection: %w", closeErr)
	}
	return nil
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

// Create 原子创建归属会话与 pending run；指定的会话必须已经存在。
func (s *RunStore) Create(ctx context.Context, convID, goal, workspace, mode string) (AgentRun, error) {
	var run AgentRun
	id, err := newID()
	if err != nil {
		return run, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if convID == "" {
			convID, err = newID()
			if err != nil {
				return err
			}
			if err := tx.Create(&Conversation{ConvID: convID}).Error; err != nil {
				return err
			}
		} else {
			var conv Conversation
			if err := tx.First(&conv, "conv_id = ?", convID).Error; err != nil {
				return fmt.Errorf("find conversation: %w", err)
			}
		}
		run = AgentRun{RunID: id, ConvID: convID, Goal: goal, Workspace: workspace, Mode: mode, Status: "pending"}
		return tx.Omit("Conversation").Create(&run).Error
	})
	if err != nil {
		return AgentRun{}, fmt.Errorf("create pending run (run the migration command first): %w", err)
	}
	return run, nil
}

func (s *RunStore) Get(ctx context.Context, runID string) (AgentRun, error) {
	defer s.lockConnection()()
	var run AgentRun
	err := s.db.WithContext(ctx).First(&run, "run_id = ?", runID).Error
	return run, err
}

// PreviousRuns 按创建顺序读取同会话较早的运行；恢复时仍以原运行的创建时间为边界。
func (s *RunStore) PreviousRuns(ctx context.Context, run *AgentRun) ([]AgentRun, error) {
	defer s.lockConnection()()
	var runs []AgentRun
	// 时间戳相同时以 run_id 固定顺序，确保不包含当前运行且恢复时顺序稳定。
	err := s.db.WithContext(ctx).Where("conv_id = ? AND run_id <> ? AND (create_at < ? OR (create_at = ? AND run_id < ?))", run.ConvID, run.RunID, run.CreateAt, run.CreateAt, run.RunID).
		Order("create_at ASC").Order("run_id ASC").Find(&runs).Error
	return runs, err
}

func (s *RunStore) Conversation(ctx context.Context, convID string) (Conversation, error) {
	defer s.lockConnection()()
	var conv Conversation
	err := s.db.WithContext(ctx).First(&conv, "conv_id = ?", convID).Error
	return conv, err
}

// Save 只更新已存在运行的 checkpoint，不能在记录丢失时悄悄重建运行。
func (s *RunStore) Save(ctx context.Context, run *AgentRun) error {
	defer s.lockConnection()()
	return saveRun(s.db.WithContext(ctx), run)
}

func saveRun(db *gorm.DB, run *AgentRun) error {
	result := db.Model(&AgentRun{}).Where("run_id = ?", run.RunID).Updates(map[string]any{
		"status": run.Status, "result": run.Result, "error": run.Error, "planning_context": run.PlanningContext,
		"mode": run.Mode, "completion_criteria": mustCriteriaJSON(run.CompletionCriteria), "update_at": time.Now(),
	})
	if result.Error != nil {
		return fmt.Errorf("save run checkpoint: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("save run checkpoint: run does not exist")
	}
	return nil
}

// SaveCheckpoint commits the run state and every supplied plan step together.
// A failed step write rolls back the run update, preventing execution from
// advancing past a checkpoint that was not durably recorded.
func (s *RunStore) SaveCheckpoint(ctx context.Context, run *AgentRun, steps []StepExecution) error {
	defer s.lockConnection()()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := saveRun(tx, run); err != nil {
			return err
		}
		for i, step := range steps {
			if step.RunID == "" {
				step.RunID = run.RunID
			}
			// JSON columns represent lists, so persist an empty list instead of
			// SQL NULL when a pending/running step has no evidence yet.
			if step.CompletionEvidence == nil {
				step.CompletionEvidence = []string{}
			}
			if step.Observation == nil {
				step.Observation = []string{}
			}
			// The slice order is the plan order.  Always persist it explicitly so
			// equal timestamps and non-sortable IDs cannot change execution order.
			step.Position = i
			// Step ID is scoped to a run; position is explicit and stable.
			result := tx.Model(&StepExecution{}).Where("run_id = ? AND step_id = ?", step.RunID, step.StepID).Updates(map[string]any{
				"status": step.Status, "intent": step.Intent, "completion_criteria": step.CompletionCriteria,
				"completion_evidence": mustListJSON(step.CompletionEvidence), "observation": mustListJSON(step.Observation),
				"position": step.Position, "revision": step.Revision, "update_at": time.Now(),
			})
			if result.Error != nil {
				return fmt.Errorf("save step checkpoint: %w", result.Error)
			}
			if result.RowsAffected == 0 {
				var existing int64
				if err := tx.Model(&StepExecution{}).Where("run_id = ? AND step_id = ?", step.RunID, step.StepID).Count(&existing).Error; err != nil {
					return fmt.Errorf("find step checkpoint: %w", err)
				}
				if existing == 0 {
					if err := tx.Create(&step).Error; err != nil {
						return fmt.Errorf("create step checkpoint: %w", err)
					}
				}
			}
		}
		return nil
	})
}

func (s *RunStore) Steps(ctx context.Context, runID string) ([]StepExecution, error) {
	defer s.lockConnection()()
	var steps []StepExecution
	err := s.db.WithContext(ctx).Where("run_id = ?", runID).Order("position ASC").Find(&steps).Error
	return steps, err
}

// SavePlanCheckpoint is a convenience adapter for callers that already have
// the ordered plan as plain database records. It preserves existing steps and
// atomically writes the supplied snapshot with the run checkpoint.
func (s *RunStore) SavePlanCheckpoint(ctx context.Context, run *AgentRun, steps []StepExecution) error {
	return s.SaveCheckpoint(ctx, run, steps)
}

// SaveDurableCheckpoint adapts the agent's public durable state to the
// database representation. The conversion stays here so both agent entries
// use the same atomic persistence path.
func (s *RunStore) SaveDurableCheckpoint(ctx context.Context, run *AgentRun, state *fileContext.DurableState, mode string) error {
	if run == nil || state == nil {
		return fmt.Errorf("save durable checkpoint: run and state are required")
	}
	// 只允许带有交接状态的 ReAct 升级；恢复配置不能降级或改写执行模式。
	if run.Mode != "" && run.Mode != mode && !(run.Mode == "react" && mode == "plan-execute" && state.PlanningContext != "") {
		return fmt.Errorf("save durable checkpoint: cannot change original mode %q to %q", run.Mode, mode)
	}
	// 事务成功后才发布新模式，避免写入失败后收尾逻辑保存半完成的切换。
	original := run
	snapshot := *run
	run = &snapshot
	run.Mode = mode
	run.Error = state.Error
	run.CompletionCriteria = state.CompletionCriteria
	run.PlanningContext = state.PlanningContext
	if state.Result != nil {
		encoded, err := json.Marshal(state.Result)
		if err != nil {
			return fmt.Errorf("save durable checkpoint: encode result: %w", err)
		}
		run.Result = encoded
	}
	steps := make([]StepExecution, len(state.StepExecutions))
	for i, step := range state.StepExecutions {
		steps[i] = StepExecution{
			StepID: step.StepId, RunID: run.RunID, Status: string(step.Status), Intent: step.Intent,
			CompletionCriteria: step.CompletionCriteria, CompletionEvidence: step.CompletionEvidence,
			Observation: step.Observation, Position: i, Revision: step.Revision,
		}
	}
	if err := s.SaveCheckpoint(ctx, run, steps); err != nil {
		return err
	}
	*original = *run
	return nil
}

func mustCriteriaJSON(criteria []string) string {
	// []string 不含 JSON 不支持的类型，编码不会失败。
	data, _ := json.Marshal(criteria)
	return string(data)
}

func mustListJSON(items []string) string {
	data, _ := json.Marshal(items)
	return string(data)
}
