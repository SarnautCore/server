package gateway

import (
	"fmt"
	"sort"
)

type Route struct {
	ZoneID         string
	ShardID        string
	PrivateAddress string
	PackID         string
	Enabled        bool
}

type Routes struct{ byZone map[string]Route }

func NewRoutes(publicPackID string, routes []Route) (Routes, error) {
	byZone := make(map[string]Route, len(routes))
	for _, route := range routes {
		if route.ZoneID == "" || route.ShardID == "" || route.PrivateAddress == "" {
			return Routes{}, errorsForRoute(route, "zone, shard, and private address must not be empty")
		}
		if route.PackID != publicPackID {
			return Routes{}, errorsForRoute(route, fmt.Sprintf("pack %q does not match public pack %q", route.PackID, publicPackID))
		}
		if _, exists := byZone[route.ZoneID]; exists {
			return Routes{}, fmt.Errorf("duplicate gateway route for zone %q", route.ZoneID)
		}
		byZone[route.ZoneID] = route
	}
	if len(byZone) == 0 {
		return Routes{}, fmt.Errorf("gateway routing table has no routes")
	}
	return Routes{byZone: byZone}, nil
}

func errorsForRoute(route Route, detail string) error {
	return fmt.Errorf("gateway route %q: %s", route.ZoneID, detail)
}

func (routes Routes) Resolve(zoneID string) (Route, error) {
	route, ok := routes.byZone[zoneID]
	if !ok || !route.Enabled {
		return Route{}, fmt.Errorf("zone %q is unavailable", zoneID)
	}
	return route, nil
}

func (routes Routes) ZoneIDs() []string {
	ids := make([]string, 0, len(routes.byZone))
	for id := range routes.byZone {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
