-- +goose Up
ALTER TABLE current_loan_position_snapshot
    ADD COLUMN loan_start_date DATE NULL;

-- +goose Down
ALTER TABLE current_loan_position_snapshot
    DROP COLUMN loan_start_date;
