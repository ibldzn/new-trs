-- +goose Up
CREATE TABLE slik_jobs (
    id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    owner_user_id BIGINT UNSIGNED NOT NULL,
    owner_username VARCHAR(191) NOT NULL,
    actor_user_id BIGINT UNSIGNED NOT NULL,
    actor_username VARCHAR(191) NOT NULL,
    original_filename VARCHAR(255) NOT NULL,
    reporting_date DATE NOT NULL,
    status VARCHAR(16) NOT NULL,
    total_rows BIGINT UNSIGNED NOT NULL,
    total_accounts BIGINT UNSIGNED NOT NULL,
    processed_accounts BIGINT UNSIGNED NOT NULL DEFAULT 0,
    created_at DATETIME(6) NOT NULL,
    started_at DATETIME(6) NULL,
    finished_at DATETIME(6) NULL,
    failed_account VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    failure_reason VARCHAR(255) NULL,
    input_file VARCHAR(64) NOT NULL,
    output_file VARCHAR(64) NULL,
    expires_at DATETIME(6) NULL,
    KEY idx_slik_queue (status, created_at, id),
    KEY idx_slik_owner (owner_user_id, created_at),
    CONSTRAINT fk_slik_job_owner FOREIGN KEY (owner_user_id) REFERENCES users(id),
    CONSTRAINT chk_slik_job_status CHECK (status IN ('QUEUED','PROCESSING','COMPLETED','FAILED','CANCELED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE slik_job_accounts (
    job_id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    requested_account VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'QUEUED',
    resolved_primary_account VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    bakidebet VARCHAR(128) NULL,
    sukubungaimbalan VARCHAR(128) NULL,
    processed_at DATETIME(6) NULL,
    PRIMARY KEY (job_id, requested_account),
    KEY idx_slik_account_status (job_id, status, requested_account),
    CONSTRAINT fk_slik_account_job FOREIGN KEY (job_id) REFERENCES slik_jobs(id) ON DELETE CASCADE,
    CONSTRAINT chk_slik_account_status CHECK (status IN ('QUEUED','COMPLETED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE slik_job_accounts;
DROP TABLE slik_jobs;
