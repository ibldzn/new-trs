package snapshot

import (
	"errors"
	"time"
)

var ErrRefreshInProgress = errors.New("snapshot refresh already in progress")

type Status struct {
	LastAttemptedAt  *time.Time `db:"last_attempted_at"`
	LastSuccessfulAt *time.Time `db:"last_successful_at"`
	LastStatus       string     `db:"last_status"`
	LastRowCount     int64      `db:"last_row_count"`
	LastErrorSummary *string    `db:"last_error_summary"`
}

type Trigger string

const (
	TriggerStartup   Trigger = "startup"
	TriggerScheduled Trigger = "scheduled"
	TriggerManual    Trigger = "manual"
)
