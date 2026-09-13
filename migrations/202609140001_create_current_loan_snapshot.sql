-- +goose Up
CREATE TABLE current_loan_position_snapshot (
    account_number VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    as_of_date DATE NOT NULL,
    principal_outstanding DECIMAL(30,10) NOT NULL,
    collectability_bi TINYINT UNSIGNED NOT NULL,
    principal_arrears DECIMAL(30,10) NOT NULL,
    interest_arrears DECIMAL(30,10) NOT NULL,
    branch VARCHAR(64) NULL,
    product VARCHAR(64) NULL,
    cif VARCHAR(64) NULL,
    contract_number VARCHAR(191) NULL,
    source_updated_at DATETIME(6) NULL,
    refreshed_at DATETIME(6) NOT NULL,
    PRIMARY KEY (account_number),
    KEY idx_current_loan_snapshot_as_of_date (as_of_date),
    CONSTRAINT chk_current_loan_snapshot_collectability CHECK (collectability_bi BETWEEN 1 AND 5),
    CONSTRAINT chk_current_loan_snapshot_balances CHECK (
        principal_outstanding >= 0 AND principal_arrears >= 0 AND interest_arrears >= 0
        AND principal_arrears <= principal_outstanding
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE snapshot_refresh_state (
    id TINYINT UNSIGNED NOT NULL,
    last_attempted_at DATETIME(6) NULL,
    last_successful_at DATETIME(6) NULL,
    last_status VARCHAR(32) NOT NULL,
    last_row_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
    last_error_summary VARCHAR(512) NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT chk_snapshot_refresh_state_singleton CHECK (id = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO snapshot_refresh_state (id, last_status, last_row_count, updated_at)
VALUES (1, 'never', 0, UTC_TIMESTAMP(6));

-- +goose Down
DROP TABLE snapshot_refresh_state;
DROP TABLE current_loan_position_snapshot;
