package recipebuilder

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry-incubator/runtime-schema/routes"
	"github.com/cloudfoundry/gunk/urljoiner"
	"github.com/pivotal-golang/lager"
)

const (
	DockerScheme = "docker"
	LRPDomain    = "cf-apps"

	MinCpuProxy = 256
	MaxCpuProxy = 8192

	DefaultFileDescriptorLimit = uint64(1024)

	LRPLogSource    = "CELL"
	AppLogSource    = "APP"
	HealthLogSource = "HEALTH"

	Router      = "router"
	DefaultPort = uint16(8080)
)

var (
	ErrNoLifecycleDefined = errors.New("no lifecycle binary bundle defined for stack")
	ErrAppSourceMissing   = errors.New("desired app missing both droplet_uri and docker_image; exactly one is required.")
	ErrMultipleAppSources = errors.New("desired app contains both droplet_uri and docker_image; exactly one is required.")
)

type RecipeBuilder struct {
	logger              lager.Logger
	lifecycles          map[string]string
	dockerLifecyclePath string
	fileServerURL       string
}

func New(lifecycles map[string]string, dockerLifecyclePath, fileServerURL string, logger lager.Logger) *RecipeBuilder {
	return &RecipeBuilder{
		lifecycles:          lifecycles,
		logger:              logger,
		dockerLifecyclePath: dockerLifecyclePath,
		fileServerURL:       fileServerURL,
	}
}

func (b *RecipeBuilder) Build(desiredApp *cc_messages.DesireAppRequestFromCC) (*receptor.DesiredLRPCreateRequest, error) {
	lrpGuid := desiredApp.ProcessGuid

	buildLogger := b.logger.Session("message-builder")

	if desiredApp.DropletUri == "" && desiredApp.DockerImageUrl == "" {
		buildLogger.Error("desired-app-invalid", ErrAppSourceMissing, lager.Data{"desired-app": desiredApp})
		return nil, ErrAppSourceMissing
	}

	if desiredApp.DropletUri != "" && desiredApp.DockerImageUrl != "" {
		buildLogger.Error("desired-app-invalid", ErrMultipleAppSources, lager.Data{"desired-app": desiredApp})
		return nil, ErrMultipleAppSources
	}

	rootFSPath := ""
	lifecycleURL := ""

	if desiredApp.DockerImageUrl != "" {
		lifecycleURL = b.lifecycleDownloadURL(b.dockerLifecyclePath, b.fileServerURL)

		rootFSPath = convertDockerURI(desiredApp.DockerImageUrl)
	} else {
		lifecyclePath, ok := b.lifecycles[desiredApp.Stack]
		if !ok {
			buildLogger.Error("unknown-stack", ErrNoLifecycleDefined, lager.Data{
				"stack": desiredApp.Stack,
			})

			return nil, ErrNoLifecycleDefined
		}

		lifecycleURL = b.lifecycleDownloadURL(lifecyclePath, b.fileServerURL)
	}

	privilegedContainer := false

	if desiredApp.DockerImageUrl == "" {
		privilegedContainer = true
	}

	numFiles := DefaultFileDescriptorLimit
	if desiredApp.FileDescriptors != 0 {
		numFiles = desiredApp.FileDescriptors
	}

	var setup []models.Action
	var action, monitor models.Action

	setup = append(setup, &models.DownloadAction{
		From: lifecycleURL,
		To:   "/tmp/lifecycle",
	})

	switch desiredApp.HealthCheckType {
	case cc_messages.PortHealthCheckType, cc_messages.UnspecifiedHealthCheckType:
		fileDescriptorLimit := DefaultFileDescriptorLimit
		monitor = &models.TimeoutAction{
			Timeout: 30 * time.Second,
			Action: &models.RunAction{
				Path:      "/tmp/lifecycle/healthcheck",
				Args:      []string{"-port=8080"},
				LogSource: HealthLogSource,
				ResourceLimits: models.ResourceLimits{
					Nofile: &fileDescriptorLimit,
				},
			},
		}
	}

	if desiredApp.DropletUri != "" {
		setup = append(setup, &models.DownloadAction{
			From:     desiredApp.DropletUri,
			To:       ".",
			CacheKey: fmt.Sprintf("droplets-%s", lrpGuid),
		})
	}

	action = &models.RunAction{
		Path: "/tmp/lifecycle/launcher",
		Args: append(
			[]string{"/app"},
			desiredApp.StartCommand,
			desiredApp.ExecutionMetadata,
		),
		Env:       createLrpEnv(desiredApp.Environment.BBSEnvironment()),
		LogSource: AppLogSource,
		ResourceLimits: models.ResourceLimits{
			Nofile: &numFiles,
		},
	}

	setupAction := models.Serial(setup...)

	desiredAppRoutes := receptor.RoutingInfo{
		CFRoutes: []receptor.CFRoute{
			{
				Port:      DefaultPort,
				Hostnames: desiredApp.Routes,
			},
		}}

	return &receptor.DesiredLRPCreateRequest{
		Privileged: privilegedContainer,

		Domain: LRPDomain,

		ProcessGuid: lrpGuid,
		Instances:   desiredApp.NumInstances,
		Routes:      &desiredAppRoutes,
		Annotation:  desiredApp.ETag,

		CPUWeight: cpuWeight(desiredApp.MemoryMB),

		MemoryMB: desiredApp.MemoryMB,
		DiskMB:   desiredApp.DiskMB,

		Ports: []uint16{
			DefaultPort,
		},

		RootFSPath: rootFSPath,

		Stack: desiredApp.Stack,

		LogGuid:   desiredApp.LogGuid,
		LogSource: LRPLogSource,

		Setup:   setupAction,
		Action:  action,
		Monitor: monitor,

		StartTimeout: desiredApp.HealthCheckTimeoutInSeconds,

		EgressRules: desiredApp.EgressRules,
	}, nil
}

func (b RecipeBuilder) lifecycleDownloadURL(lifecyclePath string, fileServerURL string) string {
	staticPath, err := routes.FileServerRoutes.CreatePathForRoute(routes.FS_STATIC, nil)
	if err != nil {
		panic("couldn't generate the download path for the bundle of app lifecycle binaries: " + err.Error())
	}

	return urljoiner.Join(fileServerURL, staticPath, lifecyclePath)
}

func createLrpEnv(env []models.EnvironmentVariable) []models.EnvironmentVariable {
	env = append(env, models.EnvironmentVariable{Name: "PORT", Value: "8080"})
	return env
}

func convertDockerURI(dockerURI string) string {
	repo, tag := parseDockerRepositoryTag(dockerURI)

	return (&url.URL{
		Scheme:   DockerScheme,
		Path:     "/" + repo,
		Fragment: tag,
	}).String()
}

// via https://github.com/docker/docker/blob/4398108/pkg/parsers/parsers.go#L72
func parseDockerRepositoryTag(repos string) (string, string) {
	n := strings.LastIndex(repos, ":")
	if n < 0 {
		return repos, ""
	}

	if tag := repos[n+1:]; !strings.Contains(tag, "/") {
		return repos[:n], tag
	}

	return repos, ""
}

func cpuWeight(memoryMB int) uint {
	cpuProxy := memoryMB

	if cpuProxy > MaxCpuProxy {
		return 100
	}

	if cpuProxy < MinCpuProxy {
		return 1
	}

	return uint(99.0*(cpuProxy-MinCpuProxy)/(MaxCpuProxy-MinCpuProxy) + 1)
}
