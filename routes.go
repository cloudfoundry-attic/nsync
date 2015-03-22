package nsync

import "github.com/tedsuo/rata"

const (
	DesireAppRoute = "Desire"
	KillIndexRoute = "KillIndex"
)

var Routes = rata.Routes{
	{Path: "/v1/apps/:process_guid", Method: "PUT", Name: DesireAppRoute},
	{Path: "/v1/apps/:process_guid/index/:index", Method: "DELETE", Name: KillIndexRoute},
}
