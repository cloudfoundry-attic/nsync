package helpers_test

import (
	"encoding/json"

	"github.com/cloudfoundry-incubator/nsync/helpers"
	"github.com/cloudfoundry-incubator/nsync/test_helpers"
	"github.com/cloudfoundry-incubator/routing-info/cfroutes"
	"github.com/cloudfoundry-incubator/routing-info/tcp_routes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Routing Helpers", func() {
	Describe("CCRouteInfo To Routes", func() {
		Context("when there are only http routes", func() {
			It("can convert itself into a Routes structure", func() {
				routeInfo, err := cc_messages.CCHTTPRoutes{
					{Hostname: "route1"},
					{Hostname: "route2", RouteServiceUrl: "https://rs.example.com"},
					{Hostname: "route3", Port: 8080},
				}.CCRouteInfo()
				Expect(err).NotTo(HaveOccurred())

				routes, err := helpers.CCRouteInfoToRoutes(routeInfo, []uint32{8080})
				Expect(err).NotTo(HaveOccurred())

				expectedCfRoutes := cfroutes.CFRoutes{
					{Hostnames: []string{"route1", "route3"}, Port: 8080},
					{Hostnames: []string{"route2"}, Port: 8080, RouteServiceUrl: "https://rs.example.com"},
				}

				test_helpers.VerifyHttpRoutes(routes, expectedCfRoutes)
				Expect(routes).To(HaveLen(1))
			})
		})

		Context("when there are only tcp routes", func() {
			It("can convert itself into a Routes structure", func() {
				routeInfo, err := cc_messages.CCTCPRoutes{
					{RouterGroupGuid: "guid-1", ExternalPort: 5222, ContainerPort: 5222},
					{RouterGroupGuid: "guid-2", ExternalPort: 1883, ContainerPort: 6000},
				}.CCRouteInfo()
				Expect(err).NotTo(HaveOccurred())

				routes, err := helpers.CCRouteInfoToRoutes(routeInfo, []uint32{5222, 6000})
				Expect(err).NotTo(HaveOccurred())

				expectedTcpRoutes := tcp_routes.TCPRoutes{
					{RouterGroupGuid: "guid-1", ExternalPort: 5222, ContainerPort: 5222},
					{RouterGroupGuid: "guid-2", ExternalPort: 1883, ContainerPort: 6000},
				}

				test_helpers.VerifyTcpRoutes(routes, expectedTcpRoutes)
				Expect(routes).To(HaveLen(1))
			})
		})

		Context("when there are both tcp and http routes", func() {
			It("can convert itself into a Routes structure", func() {
				httpRouteInfo, err := cc_messages.CCHTTPRoutes{
					{Hostname: "route1"},
					{Hostname: "route2", RouteServiceUrl: "https://rs.example.com"},
					{Hostname: "route3", Port: 8080},
				}.CCRouteInfo()

				tcpRouteInfo, err := cc_messages.CCTCPRoutes{
					{RouterGroupGuid: "guid-1", ExternalPort: 5222, ContainerPort: 5222},
					{RouterGroupGuid: "guid-2", ExternalPort: 1883, ContainerPort: 6000},
				}.CCRouteInfo()
				Expect(err).NotTo(HaveOccurred())

				routeInfo := cc_messages.CCRouteInfo{}
				routeInfo[cc_messages.CC_HTTP_ROUTES] = httpRouteInfo[cc_messages.CC_HTTP_ROUTES]
				routeInfo[cc_messages.CC_TCP_ROUTES] = tcpRouteInfo[cc_messages.CC_TCP_ROUTES]

				routes, err := helpers.CCRouteInfoToRoutes(routeInfo, []uint32{8080, 5222, 6000})
				Expect(err).NotTo(HaveOccurred())

				expectedTcpRoutes := tcp_routes.TCPRoutes{
					{RouterGroupGuid: "guid-1", ExternalPort: 5222, ContainerPort: 5222},
					{RouterGroupGuid: "guid-2", ExternalPort: 1883, ContainerPort: 6000},
				}
				test_helpers.VerifyTcpRoutes(routes, expectedTcpRoutes)

				expectedCfRoutes := cfroutes.CFRoutes{
					{Hostnames: []string{"route1", "route3"}, Port: 8080},
					{Hostnames: []string{"route2"}, Port: 8080, RouteServiceUrl: "https://rs.example.com"},
				}
				test_helpers.VerifyHttpRoutes(routes, expectedCfRoutes)
			})
		})

		Context("when CCRouteInfo is malformed", func() {
			Context("when it fails to unmarshal", func() {
				It("returns an error", func() {
					message := json.RawMessage([]byte("some random bytes"))
					routeInfo := map[string]*json.RawMessage{
						cc_messages.CC_HTTP_ROUTES: &message,
					}

					_, err := helpers.CCRouteInfoToRoutes(routeInfo, []uint32{8080})
					Expect(err).To(HaveOccurred())
				})
			})

			Context("when http routes do not contain any routes", func() {
				It("returns an empty struct", func() {

					message := json.RawMessage([]byte("[]"))
					routeInfo := map[string]*json.RawMessage{
						cc_messages.CC_HTTP_ROUTES: &message,
					}

					routes, err := helpers.CCRouteInfoToRoutes(routeInfo, []uint32{8080})
					Expect(err).NotTo(HaveOccurred())
					Expect(routes).To(HaveLen(1))
					expectedCfRoutes := cfroutes.CFRoutes{
						cfroutes.CFRoute{Hostnames: []string{}, Port: 8080},
					}
					test_helpers.VerifyHttpRoutes(routes, expectedCfRoutes)
				})
			})

			Context("when does not contain a known route type", func() {
				It("returns an empty struct", func() {

					message := json.RawMessage([]byte("some random bytes"))
					routeInfo := map[string]*json.RawMessage{
						"dummykey": &message,
					}

					routes, err := helpers.CCRouteInfoToRoutes(routeInfo, []uint32{8080})
					Expect(err).NotTo(HaveOccurred())
					Expect(routes).To(HaveLen(1))
					expectedCfRoutes := cfroutes.CFRoutes{
						cfroutes.CFRoute{Hostnames: []string{}, Port: 8080},
					}
					test_helpers.VerifyHttpRoutes(routes, expectedCfRoutes)
				})
			})
		})
	})
})
