-- +goose Up
ALTER TABLE current_loan_position_snapshot
    ADD COLUMN penalty_arrears DECIMAL(30,10) NOT NULL DEFAULT 0,
    ADD CONSTRAINT chk_current_loan_snapshot_penalty_arrears CHECK (penalty_arrears >= 0);

-- +goose Down
ALTER TABLE current_loan_position_snapshot
    DROP CHECK chk_current_loan_snapshot_penalty_arrears,
    DROP COLUMN penalty_arrears;
