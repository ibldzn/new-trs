package render

type Notice struct {
	Severity string
	Title    string
	Message  string
}

var notices = map[string]Notice{
	"user-activated":           {Severity: "success", Title: "User activated", Message: "The user can sign in again."},
	"user-deactivated":         {Severity: "success", Title: "User deactivated", Message: "The user's THOR sessions were signed out."},
	"permissions-updated":      {Severity: "success", Title: "Permissions updated", Message: "The user's explicit permissions were updated."},
	"user-status-updated":      {Severity: "success", Title: "Status updated", Message: "The user's local THOR access was updated."},
	"snapshot-refreshed":       {Severity: "success", Title: "Snapshot refreshed", Message: "Current-day positions were replaced successfully."},
	"snapshot-refresh-running": {Severity: "info", Title: "Refresh already running", Message: "Wait for the active snapshot refresh to finish."},
}

func NoticeFromID(id string) *Notice {
	notice, ok := notices[id]
	if !ok {
		return nil
	}
	return &notice
}
