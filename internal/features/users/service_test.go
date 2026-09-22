package users

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/securityctx"
)

type fakeStore struct {
	canonical, selected []string
	replaceErr          error
}

func (*fakeStore) CountUsers(context.Context, string) (int64, error) { return 0, nil }
func (*fakeStore) ListUsers(context.Context, string, int, int) ([]UserRecord, error) {
	return nil, nil
}
func (*fakeStore) FindUserByID(context.Context, uint64) (UserRecord, error) {
	return UserRecord{ID: 1, Username: "user", IsActive: true}, nil
}
func (*fakeStore) ListPermissionKeys(context.Context, uint64) ([]string, error) { return nil, nil }
func (store *fakeStore) ReplacePermissions(_ context.Context, _ securityctx.Requester, _ uint64, canonical, selected []string, _ time.Time) error {
	store.canonical, store.selected = canonical, selected
	return store.replaceErr
}
func (*fakeStore) SetUserActive(context.Context, securityctx.Requester, uint64, bool, time.Time) error {
	return nil
}

func accessDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{
		{Key: access.PermissionLoanInquiry, Name: "Loan Inquiry", Group: "Loans"},
		{Key: PermissionManage, Name: "Manage Access", Group: "Access"},
		{Key: "reporting.generate", Name: "Generate Reports", Group: "Reporting"},
	}
}

func TestBaselinePermissionIsInheritedAndCannotBeSubmitted(t *testing.T) {
	service := NewService(&fakeStore{}, accessDefinitions())
	detail, err := service.Find(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	baseline := detail.PermissionGroups[0].Permissions[0]
	if baseline.Key != access.PermissionLoanInquiry || !baseline.Selected || !baseline.Inherited {
		t.Fatalf("baseline=%+v", baseline)
	}
	if err := service.ReplacePermissions(context.Background(), securityctx.Requester{}, 1, []string{access.PermissionLoanInquiry}, time.Now()); !errors.Is(err, ErrBaselinePermission) {
		t.Fatalf("baseline mutation error=%v", err)
	}
}

func TestPermissionReplacementValidatesAndCanonicalizesKeys(t *testing.T) {
	store := &fakeStore{}
	service := NewService(store, accessDefinitions())
	if err := service.ReplacePermissions(context.Background(), securityctx.Requester{}, 1, []string{"reporting.generate", "reporting.generate"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(store.canonical) != 2 || len(store.selected) != 1 || store.selected[0] != "reporting.generate" {
		t.Fatalf("canonical=%v selected=%v", store.canonical, store.selected)
	}
	if err := service.ReplacePermissions(context.Background(), securityctx.Requester{}, 1, []string{"unknown.permission"}, time.Now()); !errors.Is(err, ErrUnknownPermission) {
		t.Fatalf("unknown mutation error=%v", err)
	}
}
