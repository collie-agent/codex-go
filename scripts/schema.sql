CREATE TABLE IF NOT EXISTS conversation (
    conv_id VARCHAR(64) PRIMARY KEY,
    create_at DATETIME(3),
    update_at DATETIME(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS agent_run (
    run_id VARCHAR(64) PRIMARY KEY,
    conv_id VARCHAR(64) NOT NULL,
    goal LONGTEXT NOT NULL,
    status VARCHAR(16) NOT NULL,
    result JSON,
    error LONGTEXT,
    mode VARCHAR(32),
    completion_criteria JSON,
    workspace LONGTEXT NOT NULL,
    create_at DATETIME(3),
    update_at DATETIME(3),
    planning_context LONGTEXT,
    multi_agent_checkpoint JSON,
    INDEX idx_agent_run_conv_id (conv_id),
    CONSTRAINT fk_agent_run_conversation FOREIGN KEY (conv_id) REFERENCES conversation(conv_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS step_execution (
    execution_id VARCHAR(64),
    step_id VARCHAR(128) NOT NULL,
    run_id VARCHAR(64) NOT NULL,
    status VARCHAR(16) NOT NULL,
    intent LONGTEXT NOT NULL,
    completion_criteria LONGTEXT NOT NULL,
    completion_evidence JSON,
    observation JSON,
    position BIGINT NOT NULL,
    revision BIGINT NOT NULL,
    create_at DATETIME(3),
    update_at DATETIME(3),
    PRIMARY KEY (step_id, run_id),
    INDEX idx_step_execution_run_id (run_id),
    CONSTRAINT fk_step_execution_agent_run FOREIGN KEY (run_id) REFERENCES agent_run(run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS execution_instance (
    execution_id VARCHAR(64) PRIMARY KEY,
    run_id VARCHAR(64),
    result JSON,
    compaction JSON,
    create_at DATETIME(3),
    update_at DATETIME(3),
    INDEX idx_execution_instance_run_id (run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS tool_execution (
    execution_id VARCHAR(64),
    call_id VARCHAR(255),
    run_id VARCHAR(64),
    position BIGINT,
    name VARCHAR(255),
    arguments LONGTEXT,
    batch BIGINT,
    batch_position BIGINT,
    status VARCHAR(32),
    result LONGTEXT,
    error LONGTEXT,
    payload JSON,
    create_at DATETIME(3),
    update_at DATETIME(3),
    PRIMARY KEY (execution_id, position),
    INDEX idx_tool_execution_run_id (run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
