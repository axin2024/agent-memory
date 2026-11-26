CREATE TABLE IF NOT EXISTS model_jobs (
 id CHAR(32) PRIMARY KEY, scope_hash CHAR(64) NOT NULL, source_hash CHAR(64) NOT NULL,
 model VARCHAR(128) NOT NULL, principal_hash CHAR(64) NOT NULL, epoch BIGINT UNSIGNED NOT NULL,
 status ENUM('queued','running','pending','no-proposal','failed','discarded') NOT NULL DEFAULT 'queued',
 attempt INT UNSIGNED NOT NULL DEFAULT 0, leased_until DATETIME(6),
 candidate CHAR(32), excerpt VARCHAR(750), prompt_tokens INT UNSIGNED NOT NULL DEFAULT 0,
 completion_tokens INT UNSIGNED NOT NULL DEFAULT 0, duration_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
 response_sha CHAR(64) NOT NULL DEFAULT '', recorded DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 UNIQUE KEY (scope_hash,source_hash,model), KEY (model,status,leased_until,recorded,id), UNIQUE KEY (scope_hash,candidate),
 CONSTRAINT model_jobs_source_fk FOREIGN KEY(scope_hash,source_hash) REFERENCES sources(scope_hash,source_hash) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
