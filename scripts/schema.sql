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
    INDEX idx_agent_run_conv_id (conv_id),
    CONSTRAINT fk_agent_run_conversation FOREIGN KEY (conv_id) REFERENCES conversation(conv_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS step_execution (
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

-- Persisted SSE events emitted for an LLM chat task.
CREATE TABLE IF NOT EXISTS `sse_stream_events` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'surrogate primary key',
    `task_id` CHAR(36) NOT NULL COMMENT 'chat task UUID',
    `last_event_id` VARCHAR(64) NOT NULL COMMENT 'source event ID, for example a Redis Stream ID',
    `data` MEDIUMTEXT NOT NULL COMMENT 'SSE data payload or model delta',
    `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'event creation time',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_sse_stream_events_task_event` (`task_id`, `last_event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='SSE streaming response event records';
