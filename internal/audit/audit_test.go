package audit

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

type recordingExecutor struct {
	sqlx.ExtContext
	args []any
}

func (executor *recordingExecutor) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	executor.args = args
	return driver.RowsAffected(1), nil
}

func TestStableActionsAreUnique(t *testing.T) {
	actions := []Action{
		ActionAuthLogin, ActionAuthLoginFailed, ActionAuthLogout, ActionAuthRegistration,
		ActionImpersonationStarted, ActionImpersonationStopped,
		ActionUserCreated, ActionUserProfileUpdated, ActionUserRoleChanged,
		ActionUserActivated, ActionUserDeactivated, ActionUserPasswordReset,
		ActionRoleCreated, ActionRoleUpdated, ActionRoleDeleted, ActionRolePermissionsUpdated,
		ActionAdminBootstrap,
		ActionLoanInquiry, ActionAPILoanLookup, ActionReportingGenerate, ActionLPSGenerate,
		ActionSnapshotRefreshStarted, ActionSnapshotRefreshSucceeded, ActionSnapshotRefreshFailed,
	}
	seen := make(map[Action]bool, len(actions))
	for _, action := range actions {
		if !knownAction(action) || seen[action] || action != Action(strings.ToLower(string(action))) {
			t.Fatalf("invalid or duplicate stable action %q", action)
		}
		seen[action] = true
	}
	if knownAction("made.up") {
		t.Fatal("unknown action accepted")
	}
}

func TestMetadataIsTypedAndSecretFree(t *testing.T) {
	metadata := []Metadata{
		RoleChangeMetadata{FromRole: "user", ToRole: "manager"},
		StatusChangeMetadata{From: "active", To: "inactive"},
		PermissionsUpdatedMetadata{Added: []string{"users.view"}, Removed: []string{"roles.view"}},
		ImpersonationStartedMetadata{TargetRole: "manager"},
		LoginFailedMetadata{Username: "member"},
		LoanInquiryMetadata{AccountNumber: "1", AsOf: "2026-09-14", Source: "DWH"},
		APILoanLookupMetadata{RequestedAccount: "1", PrimaryAccount: "2", AsOf: "2026-09-14", Outcome: "success", RequestID: "request"},
		ReportingMetadata{AsOf: "2026-09-14", RowCount: 2, Failed: 1},
		LPSMetadata{ParticipantCode: "31300082", ReportingDate: "20260914", RowCount: 3},
		SnapshotRefreshMetadata{Trigger: "manual", RowCount: 4},
	}
	for _, value := range metadata {
		typeOf := reflect.TypeOf(value)
		for index := 0; index < typeOf.NumField(); index++ {
			field := strings.ToLower(typeOf.Field(index).Name + " " + typeOf.Field(index).Tag.Get("json"))
			for _, forbidden := range []string{"password", "token", "cookie", "authorization", "credential", "hash"} {
				if strings.Contains(field, forbidden) {
					t.Fatalf("metadata field %s contains forbidden secret category %q", typeOf.Field(index).Name, forbidden)
				}
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil || !json.Valid(encoded) {
			t.Fatalf("metadata %T did not encode safely: %s %v", value, encoded, err)
		}
	}
}

func TestIdentityValidation(t *testing.T) {
	if err := validateIdentity("actor", nil); err != nil {
		t.Fatal(err)
	}
	if err := validateIdentity("actor", &Identity{UserID: 1, Username: "admin"}); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []*Identity{{Username: "admin"}, {UserID: 1}, {UserID: 1, Username: " "}} {
		if err := validateIdentity("actor", identity); err == nil {
			t.Fatalf("invalid identity accepted: %+v", identity)
		}
	}
}

func TestSystemActorAudit(t *testing.T) {
	executor := &recordingExecutor{}
	event := Event{
		Attribution: Attribution{SystemActor: "system:api"}, Action: ActionAPILoanLookup,
		Metadata:  APILoanLookupMetadata{RequestedAccount: "alternate", AsOf: "2026-09-14", Outcome: "success"},
		CreatedAt: time.Now(),
	}
	if err := Append(context.Background(), executor, event); err != nil {
		t.Fatal(err)
	}
	if executor.args[0] != nil || executor.args[1] != "system:api" || executor.args[2] != nil || executor.args[3] != nil {
		t.Fatalf("system actor columns=%v", executor.args[:4])
	}
	event.Attribution.SystemActor = "api"
	if err := Append(context.Background(), executor, event); err == nil {
		t.Fatal("unqualified system actor accepted")
	}
	event.Attribution.SystemActor = "system:api"
	event.Attribution.Actor = &Identity{UserID: 1, Username: "admin"}
	if err := Append(context.Background(), executor, event); err == nil {
		t.Fatal("system actor combined with browser user")
	}
}
