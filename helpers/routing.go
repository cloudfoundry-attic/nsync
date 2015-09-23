package helpers

import (
	"encoding/json"

	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
)

func CCRouteInfoToCFRoutes(ccRoutes cc_messages.CCRouteInfo, port uint32) (cfroutes.CFRoutes, error) {
	cfRoutes := make(cfroutes.CFRoutes, 0)

	if ccRoutes[cc_messages.CC_HTTP_ROUTES] != nil {

		var httpRoutes cc_messages.CCHTTPRoutes
		routeServiceMap := make(map[string][]string)

		err := json.Unmarshal(*ccRoutes[cc_messages.CC_HTTP_ROUTES], &httpRoutes)
		if err != nil {
			return nil, err
		}

		if len(httpRoutes) == 0 {
			cfRoutes = append(cfRoutes, cfroutes.CFRoute{Hostnames: []string{}, Port: port})
		}

		for _, httpRoute := range httpRoutes {
			list := routeServiceMap[httpRoute.RouteServiceUrl]
			routeServiceMap[httpRoute.RouteServiceUrl] = append(list, httpRoute.Hostname)
		}

		for routeServiceUrl, hostnames := range routeServiceMap {
			cfRoutes = append(cfRoutes, cfroutes.CFRoute{
				Hostnames: hostnames, Port: port, RouteServiceUrl: routeServiceUrl,
			})
		}
	} else {
		cfRoutes = append(cfRoutes, cfroutes.CFRoute{Hostnames: []string{}, Port: port})
	}

	return cfRoutes, nil
}
