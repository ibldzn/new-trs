package users

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/platform/pagination"
	"github.com/ibldzn/trs/internal/securityctx"
)

type store interface {
	CountUsers(context.Context, string) (int64, error)
	ListUsers(context.Context, string, int, int) ([]UserRecord, error)
	FindUserByID(context.Context, uint64) (UserRecord, error)
	ListPermissionKeys(context.Context, uint64) ([]string, error)
	ReplacePermissions(context.Context, securityctx.Requester, uint64, []string, []string, time.Time) error
	SetUserActive(context.Context, securityctx.Requester, uint64, bool, time.Time) error
}

type Service struct {
	store       store
	definitions []access.PermissionDefinition
}

func NewService(store store, definitions []access.PermissionDefinition) *Service {
	return &Service{store: store, definitions: append([]access.PermissionDefinition(nil), definitions...)}
}

func (service *Service) List(ctx context.Context, query string, page int) (UserPage, error) {
	query = strings.TrimSpace(query)
	total, err := service.store.CountUsers(ctx, query)
	if err != nil {
		return UserPage{}, fmt.Errorf("count access users: %w", err)
	}
	pageInfo := pagination.New(page, UserPageSize, total)
	rows, err := service.store.ListUsers(ctx, query, pageInfo.PerPage, pageInfo.Offset())
	if err != nil {
		return UserPage{}, fmt.Errorf("list access users: %w", err)
	}
	return UserPage{Users: rows, Query: query, Pagination: pageInfo}, nil
}

func (service *Service) Find(ctx context.Context, id uint64) (Detail, error) {
	if id == 0 {
		return Detail{}, ErrNotFound
	}
	found, err := service.store.FindUserByID(ctx, id)
	if err != nil {
		return Detail{}, fmt.Errorf("find access user: %w", err)
	}
	keys, err := service.store.ListPermissionKeys(ctx, id)
	if err != nil {
		return Detail{}, fmt.Errorf("list access user permissions: %w", err)
	}
	return Detail{User: found, PermissionGroups: service.groups(keys)}, nil
}

func (service *Service) ReplacePermissions(ctx context.Context, requester securityctx.Requester, userID uint64, submitted []string, now time.Time) error {
	known := make(map[string]struct{}, len(service.definitions))
	canonical := make([]string, 0, len(service.definitions))
	for _, definition := range service.definitions {
		if definition.Key == access.PermissionLoanInquiry {
			continue
		}
		known[definition.Key] = struct{}{}
		canonical = append(canonical, definition.Key)
	}
	selected := make([]string, 0, len(submitted))
	seen := make(map[string]struct{}, len(submitted))
	for _, key := range submitted {
		if key == access.PermissionLoanInquiry {
			return ErrBaselinePermission
		}
		if _, ok := known[key]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownPermission, key)
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		selected = append(selected, key)
	}
	return service.store.ReplacePermissions(ctx, requester, userID, canonical, selected, now.UTC())
}

func (service *Service) SetActive(ctx context.Context, requester securityctx.Requester, userID uint64, active bool, now time.Time) error {
	if userID == 0 {
		return ErrNotFound
	}
	return service.store.SetUserActive(ctx, requester, userID, active, now.UTC())
}

func (service *Service) groups(keys []string) []PermissionGroup {
	selected := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		selected[key] = struct{}{}
	}
	groups := make([]PermissionGroup, 0)
	indexes := map[string]int{}
	for _, definition := range service.definitions {
		index, ok := indexes[definition.Group]
		if !ok {
			index = len(groups)
			indexes[definition.Group] = index
			groups = append(groups, PermissionGroup{Name: definition.Group})
		}
		_, checked := selected[definition.Key]
		inherited := definition.Key == access.PermissionLoanInquiry
		groups[index].Permissions = append(groups[index].Permissions, PermissionOption{Key: definition.Key, Name: definition.Name, Description: definition.Description, Selected: checked || inherited, Inherited: inherited})
	}
	return groups
}
