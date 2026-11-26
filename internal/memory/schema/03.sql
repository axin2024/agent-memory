CREATE TABLE IF NOT EXISTS source_conflicts (
 id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
 scope_hash CHAR(64) NOT NULL, source_hash CHAR(64) NOT NULL, incoming_digest CHAR(64) NOT NULL,
 slot_hash CHAR(64) NOT NULL, occurred DATETIME(6) NOT NULL,
 recorded DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), dismissed DATETIME(6),
 UNIQUE KEY (scope_hash,source_hash,incoming_digest),
 KEY (scope_hash,dismissed,id), KEY (scope_hash,slot_hash,occurred),
 CONSTRAINT source_conflicts_source_fk FOREIGN KEY(scope_hash,source_hash)
   REFERENCES sources(scope_hash,source_hash) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
