package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

type Action string

const (
	// Role, password, registration, and impersonation actions remain defined only
	// so historical audit rows keep stable labels. Current runtime paths do not emit them.
	ActionAuthLoginSuccess         Action = "auth.login.success"
	ActionAuthLoginFailed          Action = "auth.login.failed"
	ActionAuthLogin                       = ActionAuthLoginSuccess
	ActionAuthLogout               Action = "auth.logout"
	ActionAuthRegistration         Action = "auth.registration"
	ActionImpersonationStarted     Action = "impersonation.started"
	ActionImpersonationStopped     Action = "impersonation.stopped"
	ActionUserCreated              Action = "user.created"
	ActionUserProfileUpdated       Action = "user.profile_updated"
	ActionUserRoleChanged          Action = "user.role_changed"
	ActionUserActivated            Action = "user.activated"
	ActionUserDeactivated          Action = "user.deactivated"
	ActionUserAutoProvisioned      Action = "user.auto_provisioned"
	ActionUserPermissionsUpdated   Action = "user.permissions_updated"
	ActionAccessBootstrap          Action = "access.bootstrap"
	ActionUserPasswordReset        Action = "user.password_reset"
	ActionRoleCreated              Action = "role.created"
	ActionRoleUpdated              Action = "role.updated"
	ActionRoleDeleted              Action = "role.deleted"
	ActionRolePermissionsUpdated   Action = "role.permissions_updated"
	ActionAdminBootstrap           Action = "admin.bootstrap"
	ActionLoanInquiry              Action = "loan.inquiry"
	ActionAPILoanLookup            Action = "api.loan_lookup"
	ActionReportingGenerate        Action = "reporting.generate"
	ActionLPSGenerate              Action = "lps.generate"
	ActionSnapshotRefreshStarted   Action = "snapshot.refresh.started"
	ActionSnapshotRefreshSucceeded Action = "snapshot.refresh.succeeded"
	ActionSnapshotRefreshFailed    Action = "snapshot.refresh.failed"
)

type ResourceType string

const (
	ResourceUser ResourceType = "user"
	ResourceRole ResourceType = "role"
)

type Identity struct {
	UserID   uint64
	Username string
}

type Attribution struct {
	Actor       *Identity
	Effective   *Identity
	SystemActor string
}

type Metadata interface {
	auditMetadata()
}

type RoleChangeMetadata struct {
	FromRole string `json:"from_role"`
	ToRole   string `json:"to_role"`
}

func (RoleChangeMetadata) auditMetadata() {}

type StatusChangeMetadata struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (StatusChangeMetadata) auditMetadata() {}

type PermissionsUpdatedMetadata struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

func (PermissionsUpdatedMetadata) auditMetadata() {}

type ImpersonationStartedMetadata struct {
	TargetRole string `json:"target_role"`
}

func (ImpersonationStartedMetadata) auditMetadata() {}

type LoginFailedMetadata struct {
	Username string `json:"username"`
}

func (LoginFailedMetadata) auditMetadata() {}

type LoanInquiryMetadata struct {
	AccountNumber string `json:"account_number"`
	AsOf          string `json:"as_of"`
	Source        string `json:"source,omitempty"`
}

func (LoanInquiryMetadata) auditMetadata() {}

type APILoanLookupMetadata struct {
	RequestedAccount string `json:"requested_account"`
	PrimaryAccount   string `json:"primary_account,omitempty"`
	AsOf             string `json:"as_of"`
	Outcome          string `json:"outcome"`
	RequestID        string `json:"request_id,omitempty"`
}

func (APILoanLookupMetadata) auditMetadata() {}

type ReportingMetadata struct {
	AsOf     string `json:"as_of"`
	RowCount int    `json:"row_count"`
	Failed   int    `json:"failed"`
}

func (ReportingMetadata) auditMetadata() {}

type LPSMetadata struct {
	ParticipantCode string `json:"participant_code"`
	ReportingDate   string `json:"reporting_date"`
	RowCount        int    `json:"row_count"`
}

func (LPSMetadata) auditMetadata() {}

type SnapshotRefreshMetadata struct {
	Trigger  string `json:"trigger"`
	RowCount int    `json:"row_count,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (SnapshotRefreshMetadata) auditMetadata() {}

type Event struct {
	Attribution Attribution
	Action      Action
	Resource    ResourceType
	ResourceID  uint64
	Metadata    Metadata
	CreatedAt   time.Time
}

type AppendFunc func(context.Context, sqlx.ExtContext, Event) error

func Append(ctx context.Context, executor sqlx.ExtContext, event Event) error {
	if executor == nil {
		return fmt.Errorf("audit executor must not be nil")
	}
	if !knownAction(event.Action) {
		return fmt.Errorf("unknown audit action %q", event.Action)
	}
	if err := validateIdentity("actor", event.Attribution.Actor); err != nil {
		return err
	}
	if err := validateIdentity("effective", event.Attribution.Effective); err != nil {
		return err
	}
	if event.Attribution.SystemActor != "" && (!strings.HasPrefix(event.Attribution.SystemActor, "system:") || event.Attribution.Actor != nil || event.Attribution.Effective != nil) {
		return fmt.Errorf("system actor must use system: prefix without user identities")
	}
	if (event.Resource == "") != (event.ResourceID == 0) {
		return fmt.Errorf("audit resource type and ID must be set together")
	}
	if event.Resource != "" && event.Resource != ResourceUser && event.Resource != ResourceRole {
		return fmt.Errorf("unknown audit resource type %q", event.Resource)
	}
	if event.CreatedAt.IsZero() {
		return fmt.Errorf("audit creation time must not be zero")
	}

	var metadata any
	if event.Metadata != nil {
		encoded, err := json.Marshal(event.Metadata)
		if err != nil {
			return fmt.Errorf("encode audit metadata: %w", err)
		}
		metadata = encoded
	}

	var actorID, actorUsername, effectiveID, effectiveUsername any
	if event.Attribution.Actor != nil {
		actorID = event.Attribution.Actor.UserID
		actorUsername = event.Attribution.Actor.Username
	}
	if event.Attribution.SystemActor != "" {
		actorUsername = event.Attribution.SystemActor
	}
	if event.Attribution.Effective != nil {
		effectiveID = event.Attribution.Effective.UserID
		effectiveUsername = event.Attribution.Effective.Username
	}
	var resourceType, resourceID any
	if event.Resource != "" {
		resourceType = event.Resource
		resourceID = event.ResourceID
	}

	const statement = `
		INSERT INTO audit_logs (
			actor_user_id, actor_username, effective_user_id, effective_username,
			action, resource_type, resource_id, metadata, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := executor.ExecContext(ctx, statement,
		actorID, actorUsername, effectiveID, effectiveUsername,
		event.Action, resourceType, resourceID, metadata, event.CreatedAt.UTC(),
	); err != nil {
		return fmt.Errorf("append audit event %q: %w", event.Action, err)
	}
	return nil
}

func validateIdentity(label string, identity *Identity) error {
	if identity == nil {
		return nil
	}
	if identity.UserID == 0 || strings.TrimSpace(identity.Username) == "" {
		return fmt.Errorf("audit %s identity must have user ID and username", label)
	}
	return nil
}

func knownAction(action Action) bool {
	switch action {
	case ActionAuthLogin,
		ActionAuthLoginFailed,
		ActionAuthLogout,
		ActionAuthRegistration,
		ActionImpersonationStarted,
		ActionImpersonationStopped,
		ActionUserCreated,
		ActionUserProfileUpdated,
		ActionUserRoleChanged,
		ActionUserActivated,
		ActionUserDeactivated,
		ActionUserAutoProvisioned,
		ActionUserPermissionsUpdated,
		ActionAccessBootstrap,
		ActionUserPasswordReset,
		ActionRoleCreated,
		ActionRoleUpdated,
		ActionRoleDeleted,
		ActionRolePermissionsUpdated,
		ActionAdminBootstrap,
		ActionLoanInquiry,
		ActionAPILoanLookup,
		ActionReportingGenerate,
		ActionLPSGenerate,
		ActionSnapshotRefreshStarted,
		ActionSnapshotRefreshSucceeded,
		ActionSnapshotRefreshFailed:
		return true
	default:
		return false
	}
}
