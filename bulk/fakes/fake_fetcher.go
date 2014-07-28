// This file was generated by counterfeiter
package fakes

import (
	"net/http"
	"sync"
	"github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
)

type FakeFetcher struct {
	FetchStub        func(chan<- models.DesireAppRequestFromCC, *http.Client) error
	fetchMutex       sync.RWMutex
	fetchArgsForCall []struct {
		arg1 chan<- models.DesireAppRequestFromCC
		arg2 *http.Client
	}
	fetchReturns struct {
		result1 error
	}
}

func (fake *FakeFetcher) Fetch(arg1 chan<- models.DesireAppRequestFromCC, arg2 *http.Client) error {
	fake.fetchMutex.Lock()
	defer fake.fetchMutex.Unlock()
	fake.fetchArgsForCall = append(fake.fetchArgsForCall, struct {
		arg1 chan<- models.DesireAppRequestFromCC
		arg2 *http.Client
	}{arg1, arg2})
	if fake.FetchStub != nil {
		return fake.FetchStub(arg1, arg2)
	} else {
		return fake.fetchReturns.result1
	}
}

func (fake *FakeFetcher) FetchCallCount() int {
	fake.fetchMutex.RLock()
	defer fake.fetchMutex.RUnlock()
	return len(fake.fetchArgsForCall)
}

func (fake *FakeFetcher) FetchArgsForCall(i int) (chan<- models.DesireAppRequestFromCC, *http.Client) {
	fake.fetchMutex.RLock()
	defer fake.fetchMutex.RUnlock()
	return fake.fetchArgsForCall[i].arg1, fake.fetchArgsForCall[i].arg2
}

func (fake *FakeFetcher) FetchReturns(result1 error) {
	fake.fetchReturns = struct {
		result1 error
	}{result1}
}

var _ bulk.Fetcher = new(FakeFetcher)