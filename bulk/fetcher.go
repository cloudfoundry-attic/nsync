package bulk

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/cloudfoundry-incubator/runtime-schema/models"
)

type Fetcher interface {
	Fetch(chan<- models.DesiredLRP, *http.Client) error
}

type CCFetcher struct {
	BaseURI   string
	BatchSize uint
	Username  string
	Password  string
}

const initialBulkToken = "{}"

func (fetcher *CCFetcher) Fetch(resultChan chan<- models.DesiredLRP, httpClient *http.Client) error {
	return fetcher.fetchBatch(initialBulkToken, resultChan, httpClient)
}

func (fetcher *CCFetcher) fetchBatch(token string, resultChan chan<- models.DesiredLRP, httpClient *http.Client) error {
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

	response := models.CCDesiredStateServerResponse{}
	err = json.Unmarshal(body, &response)
	if err != nil {
		return err
	}

	for _, app := range response.Apps {
		resultChan <- lrpFromBulkApp(app)
	}

	if uint(len(response.Apps)) < fetcher.BatchSize {
		close(resultChan)
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

func lrpFromBulkApp(app models.DesireAppRequestFromCC) models.DesiredLRP {
	return models.DesiredLRP{
		DiskMB:          int(app.DiskMB),
		Environment:     app.Environment,
		FileDescriptors: app.FileDescriptors,
		Instances:       int(app.NumInstances),
		LogGuid:         app.LogGuid,
		MemoryMB:        int(app.MemoryMB),
		ProcessGuid:     app.ProcessGuid,
		Routes:          app.Routes,
		Source:          app.DropletUri,
		Stack:           app.Stack,
		StartCommand:    app.StartCommand,
	}
}
