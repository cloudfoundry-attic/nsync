package bulk

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/cloudfoundry-incubator/runtime-schema/models"
)

type Fetcher interface {
	Fetch(chan<- models.DesiredLRP, time.Duration) error
}

type CCFetcher struct {
	BaseURI   string
	BatchSize uint
	Username  string
	Password  string
}

const initialBulkToken = "{}"

func (fetcher *CCFetcher) Fetch(resultChan chan<- models.DesiredLRP, ccFetchTimeout time.Duration) error {
	return fetcher.fetchBatch(initialBulkToken, resultChan, ccFetchTimeout)
}

func (fetcher *CCFetcher) fetchBatch(token string, resultChan chan<- models.DesiredLRP, ccFetchTimeout time.Duration) error {
	req, err := http.NewRequest("GET", fetcher.bulkURL(token), nil)
	if err != nil {
		return err
	}

	req.SetBasicAuth(fetcher.Username, fetcher.Password)

	http.DefaultClient.Timeout = ccFetchTimeout
	resp, err := http.DefaultClient.Do(req)
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

	response := DesiredStateServerResponse{}
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

	if response.BulkToken == nil {
		return fmt.Errorf("token not included in response")
	}

	return fetcher.fetchBatch(string(*response.BulkToken), resultChan, ccFetchTimeout)
}

func (fetcher *CCFetcher) bulkURL(bulkToken string) string {
	return fmt.Sprintf("%s/internal/bulk/apps?batch_size=%d&token=%s", fetcher.BaseURI, fetcher.BatchSize, bulkToken)
}

func lrpFromBulkApp(app BulkApp) models.DesiredLRP {
	environment := []models.EnvironmentVariable{}
	for _, pair := range app.Environment {
		environment = append(environment, models.EnvironmentVariable{
			Name:  pair.Name,
			Value: pair.Value,
		})
	}

	return models.DesiredLRP{
		DiskMB:          int(app.DiskMB),
		Environment:     environment,
		FileDescriptors: app.FileDescriptors,
		Instances:       int(app.Instances),
		LogGuid:         app.LogGuid,
		MemoryMB:        int(app.MemoryMB),
		ProcessGuid:     app.ProcessGuid,
		Routes:          app.Routes,
		Source:          app.SourceURL,
		Stack:           app.Stack,
		StartCommand:    app.StartCommand,
	}
}

func (response DesiredStateServerResponse) BulkTokenRepresentation() string {
	return string(*response.BulkToken)
}

type DesiredStateServerResponse struct {
	Apps      []BulkApp        `json:"apps"`
	BulkToken *json.RawMessage `json:"token"`
}

type BulkApp struct {
	DiskMB      uint64 `json:"disk_mb"`
	Environment []struct {
		Name  string
		Value string
	} `json:"environment"`
	FileDescriptors uint64   `json:"file_descriptors"`
	Instances       uint     `json:"instances"`
	LogGuid         string   `json:"log_guid"`
	MemoryMB        uint64   `json:"memory_mb"`
	ProcessGuid     string   `json:"process_guid"`
	Routes          []string `json:"routes"`
	SourceURL       string   `json:"source_url"`
	Stack           string   `json:"stack"`
	StartCommand    string   `json:"start_command"`
}

type BulkToken struct {
	Id int `json:"id"`
}
