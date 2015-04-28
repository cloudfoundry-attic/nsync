package handlers_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/cloudfoundry-incubator/nsync/handlers"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/receptor/fake_receptor"
	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("StopAppHandler", func() {
	var (
		logger       *lagertest.TestLogger
		fakeReceptor *fake_receptor.FakeClient

		request          *http.Request
		responseRecorder *httptest.ResponseRecorder
	)

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")
		fakeReceptor = new(fake_receptor.FakeClient)

		responseRecorder = httptest.NewRecorder()

		var err error
		request, err = http.NewRequest("DELETE", "", nil)
		Expect(err).NotTo(HaveOccurred())
		request.Form = url.Values{
			":process_guid": []string{"process-guid"},
		}
	})

	JustBeforeEach(func() {
		stopAppHandler := handlers.NewStopAppHandler(logger, fakeReceptor)
		stopAppHandler.StopApp(responseRecorder, request)
	})

	It("invokes the receptor to delete the app", func() {
		Expect(fakeReceptor.DeleteDesiredLRPCallCount()).To(Equal(1))
		Expect(fakeReceptor.DeleteDesiredLRPArgsForCall(0)).To(Equal("process-guid"))
	})

	It("responds with 202 Accepted", func() {
		Expect(responseRecorder.Code).To(Equal(http.StatusAccepted))
	})

	Context("when the receptor fails", func() {
		BeforeEach(func() {
			fakeReceptor.DeleteDesiredLRPReturns(errors.New("oh no"))
		})

		It("responds with a ServiceUnavailabe error", func() {
			Expect(responseRecorder.Code).To(Equal(http.StatusServiceUnavailable))
		})
	})

	Context("when the process guid is missing", func() {
		BeforeEach(func() {
			request.Form.Del(":process_guid")
		})

		It("does not call the receptor", func() {
			Expect(fakeReceptor.DeleteDesiredLRPCallCount()).To(Equal(0))
		})

		It("responds with 400 Bad Request", func() {
			Expect(responseRecorder.Code).To(Equal(http.StatusBadRequest))
		})
	})

	Context("when the lrp doesn't exist", func() {
		BeforeEach(func() {
			fakeReceptor.DeleteDesiredLRPReturns(receptor.Error{
				Type:    receptor.DesiredLRPNotFound,
				Message: "Desired LRP with guid 'process-guid' not found",
			})
		})

		It("responds with a 404", func() {
			Expect(responseRecorder.Code).To(Equal(http.StatusNotFound))
		})
	})
})
