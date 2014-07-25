package recipebuilder

import (
	"errors"
	"fmt"
	"strings"

	RepRoutes "github.com/cloudfoundry-incubator/rep/routes"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	SchemaRouter "github.com/cloudfoundry-incubator/runtime-schema/router"
	"github.com/cloudfoundry/gunk/urljoiner"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/rata"
)

var ErrNoCircusDefined = errors.New("no lifecycle binary bundle defined for stack")

type RecipeBuilder struct {
	repAddrRelativeToExecutor string
	logger                    lager.Logger
	circuses                  map[string]string
}

func New(repAddrRelativeToExecutor string, circuses map[string]string, logger lager.Logger) *RecipeBuilder {
	return &RecipeBuilder{
		repAddrRelativeToExecutor: repAddrRelativeToExecutor,
		circuses:                  circuses,
		logger:                    logger,
	}
}

func (b *RecipeBuilder) Build(desiredApp models.DesireAppRequestFromCC) (models.DesiredLRP, error) {
	lrpGuid := desiredApp.ProcessGuid

	buildLogger := b.logger.Session("message-builder")

	circusURL, err := b.circusDownloadURL(desiredApp.Stack, "http://PLACEHOLDER_FILESERVER_ADDR")
	if err != nil {
		buildLogger.Error("construct-circus-download-url-failed", err, lager.Data{
			"stack": desiredApp.Stack,
		})

		return models.DesiredLRP{}, err
	}

	var numFiles *uint64
	if desiredApp.FileDescriptors != 0 {
		numFiles = &desiredApp.FileDescriptors
	}

	repRequests := rata.NewRequestGenerator(
		"http://"+b.repAddrRelativeToExecutor,
		RepRoutes.Routes,
	)

	healthyHook, err := repRequests.CreateRequest(
		RepRoutes.LRPRunning,
		rata.Params{
			"process_guid": lrpGuid,

			// these go away once rep is polling, rather than receiving callbacks
			"index":         "PLACEHOLDER_INDEX",
			"instance_guid": "PLACEHOLDER_INSTANCE_GUID",
		},
		nil,
	)
	if err != nil {
		return models.DesiredLRP{}, err
	}

	return models.DesiredLRP{
		ProcessGuid: lrpGuid,
		Instances:   desiredApp.NumInstances,
		Routes:      desiredApp.Routes,

		MemoryMB: desiredApp.MemoryMB,
		DiskMB:   desiredApp.DiskMB,

		Ports: []models.PortMapping{
			{ContainerPort: 8080},
		},

		Stack: desiredApp.Stack,

		Log: models.LogConfig{
			Guid:       desiredApp.LogGuid,
			SourceName: "App",
		},

		Actions: []models.ExecutorAction{
			{
				Action: models.DownloadAction{
					From:    circusURL,
					To:      "/tmp/circus",
					Extract: true,
				},
			},
			{
				Action: models.DownloadAction{
					From:     desiredApp.DropletUri,
					To:       ".",
					Extract:  true,
					CacheKey: fmt.Sprintf("droplets-%s", lrpGuid),
				},
			},
			models.Parallel(
				models.ExecutorAction{
					models.RunAction{
						Path:    "/tmp/circus/soldier",
						Args:    append([]string{"/app"}, strings.Split(desiredApp.StartCommand, " ")...),
						Env:     createLrpEnv(desiredApp.Environment),
						Timeout: 0,
						ResourceLimits: models.ResourceLimits{
							Nofile: numFiles,
						},
					},
				},
				models.ExecutorAction{
					models.MonitorAction{
						Action: models.ExecutorAction{
							models.RunAction{
								Path: "/tmp/circus/spy",
								Args: []string{"-addr=:8080"},
							},
						},
						HealthyThreshold:   1,
						UnhealthyThreshold: 1,
						HealthyHook: models.HealthRequest{
							Method: healthyHook.Method,
							URL:    healthyHook.URL.String(),
						},
					},
				},
			),
		},
	}, nil
}

func (b RecipeBuilder) circusDownloadURL(stack string, fileServerURL string) (string, error) {
	checkPath, ok := b.circuses[stack]
	if !ok {
		return "", ErrNoCircusDefined
	}

	staticRoute, ok := SchemaRouter.NewFileServerRoutes().RouteForHandler(SchemaRouter.FS_STATIC)
	if !ok {
		return "", errors.New("couldn't generate the download path for the bundle of app lifecycle binaries")
	}

	return urljoiner.Join(fileServerURL, staticRoute.Path, checkPath), nil
}

func createLrpEnv(env []models.EnvironmentVariable) []models.EnvironmentVariable {
	env = append(env, models.EnvironmentVariable{Name: "PORT", Value: "8080"})
	env = append(env, models.EnvironmentVariable{Name: "VCAP_APP_PORT", Value: "8080"})
	env = append(env, models.EnvironmentVariable{Name: "VCAP_APP_HOST", Value: "0.0.0.0"})
	return env
}
