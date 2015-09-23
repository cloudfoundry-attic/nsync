package helpers_test

import (
	"encoding/json"

	"github.com/cloudfoundry-incubator/nsync/helpers"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Routing Helpers", func() {
	Describe("CCRouteInfo To CFRoutes", func() {
		It("can convert itself into a CFRoutes structure", func() {
			routeInfo, err := cc_messages.CCHTTPRoutes{
				{Hostname: "route1"},
				{Hostname: "route2", RouteServiceUrl: "https://rs.example.com"},
				{Hostname: "route3"},
			}.CCRouteInfo()
			Expect(err).NotTo(HaveOccurred())

			cfRoutes, err := helpers.CCRouteInfoToCFRoutes(routeInfo, 8080)
			Expect(err).NotTo(HaveOccurred())

			expectedRoutes := cfroutes.CFRoutes{
				{Hostnames: []string{"route1", "route3"}, Port: 8080},
				{Hostnames: []string{"route2"}, Port: 8080, RouteServiceUrl: "https://rs.example.com"},
			}

			Expect(cfRoutes).To(ConsistOf(expectedRoutes))
			Expect(cfRoutes).To(HaveLen(len(expectedRoutes)))
		})

		Context("when CCRouteInfo is malformed", func() {
			Context("when it fails to unmarshal", func() {
				It("returns an error", func() {
					message := json.RawMessage([]byte("some random bytes"))
					routeInfo := map[string]*json.RawMessage{
						cc_messages.CC_HTTP_ROUTES: &message,
					}

					_, err := helpers.CCRouteInfoToCFRoutes(routeInfo, 8080)
					Expect(err).To(HaveOccurred())
				})
			})

			Context("when does not contain a known route type", func() {
				It("returns an empty struct", func() {

					message := json.RawMessage([]byte("some random bytes"))
					routeInfo := map[string]*json.RawMessage{
						"dummykey": &message,
					}

					cfRoutes, err := helpers.CCRouteInfoToCFRoutes(routeInfo, 8080)
					Expect(err).NotTo(HaveOccurred())
					Expect(cfRoutes).To(HaveLen(1))
					Expect(cfRoutes[0]).To(Equal(cfroutes.CFRoute{Hostnames: []string{}, Port: 8080}))
				})
			})
		})
	})
})
