package snapshots

import (
	"context"
	"errors"
	"net/http"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	"github.com/ibldzn/trs/internal/snapshot"
)

type snapshotService interface {
	Status(context.Context) (snapshot.Status, error)
	Refresh(context.Context, snapshot.Trigger, audit.Attribution) (int, error)
}

type Handler struct {
	admin   *adminshell.Shell
	service snapshotService
}

type PageData struct {
	Status     snapshot.Status
	CanRefresh bool
	Error      string
}

func NewHandler(admin *adminshell.Shell, service snapshotService) *Handler {
	return &Handler{admin: admin, service: service}
}

func (handler *Handler) Index(writer http.ResponseWriter, request *http.Request) {
	status, err := handler.service.Status(request.Context())
	if err != nil {
		handler.admin.Internal(writer, request, "read snapshot status", err)
		return
	}
	principal, _ := browserauth.CurrentPrincipal(request.Context())
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/snapshots/index", "Current Snapshot", PageData{Status: status, CanRefresh: principal.Can(PermissionRefresh)})
}

func (handler *Handler) Refresh(writer http.ResponseWriter, request *http.Request) {
	principal, ok := browserauth.CurrentPrincipal(request.Context())
	if !ok {
		handler.admin.Internal(writer, request, "manual snapshot refresh", errors.New("principal missing"))
		return
	}
	actor := audit.Identity{UserID: principal.Actor.UserID, Username: principal.Actor.Username}
	effective := audit.Identity{UserID: principal.UserID, Username: principal.Username}
	_, err := handler.service.Refresh(request.Context(), snapshot.TriggerManual, audit.Attribution{Actor: &actor, Effective: &effective})
	if errors.Is(err, snapshot.ErrRefreshInProgress) {
		http.Redirect(writer, request, "/snapshot?notice=snapshot-refresh-running", http.StatusSeeOther)
		return
	}
	if err != nil {
		status, statusErr := handler.service.Status(request.Context())
		if statusErr != nil {
			handler.admin.Internal(writer, request, "read failed snapshot status", statusErr)
			return
		}
		handler.admin.RenderPage(writer, request, http.StatusServiceUnavailable, "features/snapshots/index", "Current Snapshot", PageData{Status: status, CanRefresh: true, Error: "Snapshot refresh failed; the previous successful dataset was preserved."})
		return
	}
	http.Redirect(writer, request, "/snapshot?notice=snapshot-refreshed", http.StatusSeeOther)
}
