package bulk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/pivotal-golang/lager"
)

//go:generate counterfeiter -o fakes/fake_fetcher.go . Fetcher
type Fetcher interface {
	FetchFingerprints(
		logger lager.Logger,
		cancel <-chan struct{},
		desiredAppFingerprints chan<- []cc_messages.CCDesiredAppFingerprint,
		httpClient *http.Client,
	) error

	FetchDesiredLRPs(
		logger lager.Logger,
		cancel <-chan struct{},
		desiredAppFingerprints <-chan []cc_messages.CCDesiredAppFingerprint,
		desireAppRequestsFromCC chan<- []cc_messages.DesireAppRequestFromCC,
		httpClient *http.Client,
	) error
}

type CCFetcher struct {
	BaseURI   string
	BatchSize int
	Username  string
	Password  string
}

const initialBulkToken = "{}"

func (fetcher *CCFetcher) FetchFingerprints(
	logger lager.Logger,
	cancel <-chan struct{},
	resultChan chan<- []cc_messages.CCDesiredAppFingerprint,
	httpClient *http.Client,
) error {
	// ensure this happens regardless of success or failure;
	defer close(resultChan)

	logger = logger.Session("fetch-fingerprints-from-cc")

	token := initialBulkToken

	for {
		logger.Info("fetching-desired", lager.Data{"token": token})

		req, err := http.NewRequest("GET", fetcher.fingerprintURL(token), nil)
		if err != nil {
			return err
		}

		response := cc_messages.CCDesiredStateFingerprintResponse{}

		err = fetcher.doRequest(logger, httpClient, req, &response)
		if err != nil {
			return err
		}

		select {
		case resultChan <- response.Fingerprints:
		case <-cancel:
			return nil
		}

		if len(response.Fingerprints) < fetcher.BatchSize {
			return nil
		}

		if response.CCBulkToken == nil {
			return errors.New("token not included in response")
		}

		token = string(*response.CCBulkToken)
	}
}

func (fetcher *CCFetcher) FetchDesiredLRPs(
	logger lager.Logger,
	cancel <-chan struct{},
	fingerprintCh <-chan []cc_messages.CCDesiredAppFingerprint,
	desiredStateCh chan<- []cc_messages.DesireAppRequestFromCC,
	httpClient *http.Client,
) error {
	// ensure this happens regardless of success or failure;
	defer close(desiredStateCh)

	logger = logger.Session("fetch-desired-lrps-from-cc")

	for {
		var fingerprints []cc_messages.CCDesiredAppFingerprint

		select {
		case <-cancel:
			return nil
		case selected, ok := <-fingerprintCh:
			if !ok {
				return nil
			}
			fingerprints = selected
		}

		if len(fingerprints) == 0 {
			continue
		}

		processGuids := make([]string, len(fingerprints))
		for i, fingerprint := range fingerprints {
			processGuids[i] = fingerprint.ProcessGuid
		}

		payload, err := json.Marshal(processGuids)
		if err != nil {
			return err
		}

		logger.Info("fetching-desired", lager.Data{"fingerprints-length": len(fingerprints)})

		req, err := http.NewRequest("POST", fetcher.desiredURL(), bytes.NewReader(payload))
		if err != nil {
			return err
		}

		response := []cc_messages.DesireAppRequestFromCC{}
		err = fetcher.doRequest(logger, httpClient, req, &response)
		if err != nil {
			continue
		}

		select {
		case desiredStateCh <- response:
		case <-cancel:
			return nil
		}
	}

	return nil
}

func (fetcher *CCFetcher) doRequest(
	logger lager.Logger,
	httpClient *http.Client,
	req *http.Request,
	value interface{},
) error {
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(fetcher.Username, fetcher.Password)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	logger.Info("fetching-desired-complete", lager.Data{
		"StatusCode": resp.StatusCode,
	})

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid response code %d", resp.StatusCode)
	}

	err = json.NewDecoder(resp.Body).Decode(value)
	if err != nil {
		logger.Error("decode-body", err)
		return err
	}

	return nil
}

func (fetcher *CCFetcher) fingerprintURL(bulkToken string) string {
	return fmt.Sprintf("%s/internal/bulk/apps?batch_size=%d&format=fingerprint&token=%s", fetcher.BaseURI, fetcher.BatchSize, bulkToken)
}

func (fetcher *CCFetcher) desiredURL() string {
	return fmt.Sprintf("%s/internal/bulk/apps", fetcher.BaseURI)
}
