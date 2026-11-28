CREATE TABLE IF NOT EXISTS team_spaces (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, owner VARCHAR(48) NOT NULL,
 label VARCHAR(160) NOT NULL, policy_rev BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY (tenant,space_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_purposes (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, purpose VARCHAR(48) NOT NULL, active BOOLEAN NOT NULL,
 PRIMARY KEY (tenant,space_id,purpose),
 FOREIGN KEY (tenant,space_id) REFERENCES team_spaces(tenant,space_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_memberships (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, purpose VARCHAR(48) NOT NULL, actor VARCHAR(48) NOT NULL,
 role ENUM('reader','author','reviewer','maintainer') NOT NULL, can_export BOOLEAN NOT NULL, active BOOLEAN NOT NULL,
 PRIMARY KEY (tenant,space_id,purpose,actor), KEY team_member_spaces(tenant,actor,active,space_id,purpose),
 FOREIGN KEY (tenant,space_id,purpose) REFERENCES team_purposes(tenant,space_id,purpose)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_nominations (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, purpose VARCHAR(48) NOT NULL, nomination CHAR(32) NOT NULL,
 actor VARCHAR(48) NOT NULL, source_scope CHAR(64) NOT NULL, source_hash CHAR(64) NOT NULL,
 source_digest CHAR(64) NOT NULL, grant_rev BIGINT UNSIGNED NOT NULL, start_byte SMALLINT UNSIGNED NOT NULL,
 end_byte SMALLINT UNSIGNED NOT NULL, allow_model BOOLEAN NOT NULL, expires DATETIME(6) NOT NULL,
 revoked_seq BIGINT UNSIGNED NULL, submitted DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (tenant,space_id,nomination), UNIQUE KEY team_nomination_version(tenant,space_id,purpose,source_hash,grant_rev),
 KEY team_nomination_source(source_scope,source_hash,revoked_seq), UNIQUE KEY team_nomination_reference(nomination,source_scope),
 FOREIGN KEY (tenant,space_id,purpose) REFERENCES team_purposes(tenant,space_id,purpose)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_claims (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, claim_id CHAR(32) NOT NULL, purpose VARCHAR(48) NOT NULL,
 claim_rev BIGINT UNSIGNED NOT NULL, author VARCHAR(48) NOT NULL, topic VARCHAR(64) NOT NULL, applicability VARCHAR(220) NOT NULL,
 content TEXT, valid_from DATETIME(6) NOT NULL, expires DATETIME(6) NOT NULL,
 review_state ENUM('awaiting_review','disputed','published','rejected','superseded','withdrawn') NOT NULL,
 submitted DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), published DATETIME(6),
 supersedes CHAR(32), withdrawn_seq BIGINT UNSIGNED, idempotency_key VARCHAR(80) NOT NULL, input_digest CHAR(64) NOT NULL,
 model_provenance JSON, review_reason VARCHAR(300), review_queue BOOLEAN NOT NULL DEFAULT FALSE, review_owner VARCHAR(48),
 PRIMARY KEY (tenant,space_id,claim_id), UNIQUE KEY team_claim_key(tenant,space_id,author,idempotency_key),
 KEY team_claim_search(tenant,space_id,purpose,review_state,expires,claim_id),
 KEY team_claim_topic(tenant,space_id,purpose,topic,review_state), KEY team_claim_successor(tenant,space_id,supersedes),
 FOREIGN KEY (tenant,space_id,purpose) REFERENCES team_purposes(tenant,space_id,purpose),
 FOREIGN KEY (tenant,space_id,supersedes) REFERENCES team_claims(tenant,space_id,claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_claim_evidence (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, claim_id CHAR(32) NOT NULL, sentence SMALLINT UNSIGNED NOT NULL,
 nomination CHAR(32) NOT NULL, start_byte SMALLINT UNSIGNED NOT NULL, end_byte SMALLINT UNSIGNED NOT NULL,
 PRIMARY KEY (tenant,space_id,claim_id,sentence,nomination,start_byte),
 KEY team_edges_nomination(tenant,space_id,nomination,claim_id),
 FOREIGN KEY (tenant,space_id,claim_id) REFERENCES team_claims(tenant,space_id,claim_id),
 FOREIGN KEY (tenant,space_id,nomination) REFERENCES team_nominations(tenant,space_id,nomination)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_votes (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, claim_id CHAR(32) NOT NULL, actor VARCHAR(48) NOT NULL,
 decision ENUM('approve','dispute','reject') NOT NULL, reason VARCHAR(300) NOT NULL, created DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (tenant,space_id,claim_id,actor),
 FOREIGN KEY (tenant,space_id,claim_id) REFERENCES team_claims(tenant,space_id,claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_search_terms (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, purpose VARCHAR(48) NOT NULL,
 term VARCHAR(64) NOT NULL, claim_id CHAR(32) NOT NULL, weight TINYINT UNSIGNED NOT NULL,
 PRIMARY KEY (tenant,space_id,purpose,term,claim_id), KEY team_terms_claim(tenant,space_id,claim_id),
 FOREIGN KEY (tenant,space_id,claim_id) REFERENCES team_claims(tenant,space_id,claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_vectors (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, purpose VARCHAR(48) NOT NULL,
 model_hash CHAR(64) NOT NULL, bucket INT UNSIGNED NOT NULL, claim_id CHAR(32) NOT NULL, vector BLOB NOT NULL,
 PRIMARY KEY (tenant,space_id,purpose,model_hash,bucket,claim_id), KEY team_vectors_claim(tenant,space_id,claim_id),
 FOREIGN KEY (tenant,space_id,claim_id) REFERENCES team_claims(tenant,space_id,claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_watermarks (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, withdrawal_seq BIGINT UNSIGNED NOT NULL, indexed_seq BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY (tenant,space_id), FOREIGN KEY (tenant,space_id) REFERENCES team_spaces(tenant,space_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_withdrawals (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, seq BIGINT UNSIGNED NOT NULL, claim_id CHAR(32) NOT NULL,
 PRIMARY KEY (tenant,space_id,seq,claim_id),
 FOREIGN KEY (tenant,space_id,claim_id) REFERENCES team_claims(tenant,space_id,claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
CREATE TABLE IF NOT EXISTS team_evaluations (
 tenant VARCHAR(48) NOT NULL, space_id VARCHAR(48) NOT NULL, evaluation CHAR(32) NOT NULL, actor VARCHAR(48) NOT NULL,
 frozen_digest CHAR(64) NOT NULL, input JSON NOT NULL, report JSON NOT NULL, created DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY (tenant,space_id,evaluation), FOREIGN KEY (tenant,space_id) REFERENCES team_spaces(tenant,space_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
