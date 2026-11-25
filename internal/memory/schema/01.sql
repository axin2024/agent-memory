CREATE TABLE IF NOT EXISTS subjects (
 tenant VARCHAR(48) NOT NULL, subject VARCHAR(48) NOT NULL, scope_hash CHAR(64) NOT NULL,
 epoch BIGINT UNSIGNED NOT NULL DEFAULT 0, PRIMARY KEY (tenant,subject), UNIQUE KEY (scope_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS principals (
 token CHAR(64) PRIMARY KEY, tenant VARCHAR(48) NOT NULL, actor VARCHAR(48) NOT NULL,
 role ENUM('owner','app','admin','audit') NOT NULL, expires DATETIME(6) NOT NULL,
 KEY (tenant,actor,role)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS policy (
 tenant VARCHAR(48) PRIMARY KEY, source_days SMALLINT UNSIGNED NOT NULL,
 fact_days SMALLINT UNSIGNED NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS consent (
 scope_hash CHAR(64) NOT NULL, app VARCHAR(48) NOT NULL, purpose VARCHAR(48) NOT NULL,
 grant_hash CHAR(64) NOT NULL, can_read BOOLEAN NOT NULL, can_write BOOLEAN NOT NULL,
 auto_accept BOOLEAN NOT NULL, allow_model BOOLEAN NOT NULL, expires DATETIME(6) NOT NULL,
 PRIMARY KEY (scope_hash,app,purpose), UNIQUE KEY (scope_hash,grant_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS sources (
 scope_hash CHAR(64) NOT NULL, source_hash CHAR(64) NOT NULL, digest CHAR(64) NOT NULL,
 app VARCHAR(48) NOT NULL, purpose VARCHAR(48) NOT NULL,
 session_id VARCHAR(48) NOT NULL, external_id VARCHAR(48) NOT NULL,
 occurred DATETIME(6) NOT NULL, expires DATETIME(6) NOT NULL,
 kind ENUM('user','quotation','transient','sensitive') NOT NULL, statement VARCHAR(750),
 slot VARCHAR(48), slot_hash CHAR(64), PRIMARY KEY (scope_hash,source_hash), KEY (scope_hash,slot_hash,occurred),
 KEY (scope_hash,expires)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS candidates (
 scope_hash CHAR(64) NOT NULL, candidate CHAR(32) NOT NULL,
 source_hash CHAR(64) NOT NULL, slot VARCHAR(48) NOT NULL, slot_hash CHAR(64) NOT NULL,
 value VARCHAR(240) NOT NULL, status ENUM('pending','applied','conflict','rejected','stale') NOT NULL,
 extraction ENUM('rule','model','manual') NOT NULL, PRIMARY KEY (scope_hash,candidate),
 UNIQUE KEY (scope_hash,source_hash), KEY (scope_hash,slot_hash,status),
 CONSTRAINT candidates_source_fk FOREIGN KEY(scope_hash, source_hash) REFERENCES sources(scope_hash,source_hash) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS facts (
 scope_hash CHAR(64) NOT NULL, slot VARCHAR(48) NOT NULL, slot_hash CHAR(64) NOT NULL, version BIGINT UNSIGNED NOT NULL,
 value VARCHAR(240) NOT NULL, source_hash CHAR(64) NOT NULL, app VARCHAR(48) NOT NULL,
 purpose VARCHAR(48) NOT NULL, session_id VARCHAR(48) NOT NULL, external_id VARCHAR(48) NOT NULL,
 evidence VARCHAR(750), effective DATETIME(6) NOT NULL, replaced DATETIME(6), expires DATETIME(6) NOT NULL,
 authority TINYINT UNSIGNED NOT NULL, PRIMARY KEY (scope_hash,slot_hash,purpose,version),
 KEY (scope_hash,purpose,effective,replaced,expires),
 KEY (scope_hash,expires)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS shares (
 share_hash CHAR(64) PRIMARY KEY, scope_hash CHAR(64) NOT NULL, slot_hash CHAR(64) NOT NULL,
 purpose VARCHAR(48) NOT NULL, epoch BIGINT UNSIGNED NOT NULL, version BIGINT UNSIGNED NOT NULL, expires DATETIME(6) NOT NULL,
 KEY (scope_hash,expires)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS journal_state (
 singleton TINYINT PRIMARY KEY, seq BIGINT UNSIGNED NOT NULL, mac CHAR(64) NOT NULL, identity CHAR(32) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
INSERT IGNORE INTO journal_state VALUES (1,0,'','');
CREATE TABLE IF NOT EXISTS forget_cuts (
 scope_hash CHAR(64) NOT NULL, selector CHAR(64) NOT NULL, cutoff DATETIME(6) NOT NULL,
 PRIMARY KEY (scope_hash,selector)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS forgotten_sources (
 scope_hash CHAR(64) NOT NULL, source_hash CHAR(64) NOT NULL,
 PRIMARY KEY (scope_hash,source_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
