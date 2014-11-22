package bulk

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
)

type Fetcher interface {
	Fetch(chan<- *cc_messages.DesireAppRequestFromCC, *http.Client) error
}

type CCFetcher struct {
	BaseURI   string
	BatchSize uint
	Username  string
	Password  string
}

const initialBulkToken = "{}"

func (fetcher *CCFetcher) Fetch(resultChan chan<- *cc_messages.DesireAppRequestFromCC, httpClient *http.Client) error {
	// ensure this happens regardless of success or failure;
	// don't do this in fetchBatch as fetchBatch is recursive
	defer close(resultChan)

	return fetcher.fetchBatch(initialBulkToken, resultChan, httpClient)
}

func (fetcher *CCFetcher) fetchBatch(token string, resultChan chan<- *cc_messages.DesireAppRequestFromCC, httpClient *http.Client) error {
	req, err := http.NewRequest("GET", fetcher.bulkURL(token), nil)
	if err != nil {
		return err
	}

	req.SetBasicAuth(fetcher.Username, fetcher.Password)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid response code %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	response := cc_messages.CCDesiredStateServerResponse{}
	err = json.Unmarshal(body, &response)
	if err != nil {
		return err
	}

	for i, _ := range response.Apps {
		resultChan <- &response.Apps[i]
	}

	if uint(len(response.Apps)) < fetcher.BatchSize {
		return nil
	}

	if response.CCBulkToken == nil {
		return fmt.Errorf("token not included in response")
	}

	return fetcher.fetchBatch(string(*response.CCBulkToken), resultChan, httpClient)
}

func (fetcher *CCFetcher) bulkURL(bulkToken string) string {
	return fmt.Sprintf("%s/internal/bulk/apps?batch_size=%d&token=%s", fetcher.BaseURI, fetcher.BatchSize, bulkToken)
}
