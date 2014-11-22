package recipebuilder

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	SchemaRouter "github.com/cloudfoundry-incubator/runtime-schema/router"
	"github.com/cloudfoundry/gunk/urljoiner"
	"github.com/pivotal-golang/lager"
)

const (
	DockerScheme = "docker"
	LRPDomain    = "cf-apps"

	MinCpuProxy = 256
	MaxCpuProxy = 8192

	LRPLogSource    = "CELL"
	AppLogSource    = "APP"
	HealthLogSource = "HEALTH"
)

var (
	ErrNoCircusDefined    = errors.New("no lifecycle binary bundle defined for stack")
	ErrAppSourceMissing   = errors.New("desired app missing both droplet_uri and docker_image; exactly one is required.")
	ErrMultipleAppSources = errors.New("desired app contains both droplet_uri and docker_image; exactly one is required.")
)

type RecipeBuilder struct {
	logger           lager.Logger
	circuses         map[string]string
	dockerCircusPath string
	fileServerURL    string
}

func New(circuses map[string]string, dockerCircusPath, fileServerURL string, logger lager.Logger) *RecipeBuilder {
	return &RecipeBuilder{
		circuses:         circuses,
		logger:           logger,
		dockerCircusPath: dockerCircusPath,
		fileServerURL:    fileServerURL,
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
	circusURL := ""

	if desiredApp.DockerImageUrl != "" {
		circusURL = b.circusDownloadURL(b.dockerCircusPath, b.fileServerURL)

		rootFSPath = convertDockerURI(desiredApp.DockerImageUrl)
	} else {
		circusPath, ok := b.circuses[desiredApp.Stack]
		if !ok {
			buildLogger.Error("unknown-stack", ErrNoCircusDefined, lager.Data{
				"stack": desiredApp.Stack,
			})

			return nil, ErrNoCircusDefined
		}

		circusURL = b.circusDownloadURL(circusPath, b.fileServerURL)
	}

	var numFiles *uint64
	if desiredApp.FileDescriptors != 0 {
		numFiles = &desiredApp.FileDescriptors
	}

	var setup []models.Action
	var action, monitor models.Action

	setup = append(setup, &models.DownloadAction{
		From: circusURL,
		To:   "/tmp/circus",
	})

	if desiredApp.DropletUri != "" {
		setup = append(setup, &models.DownloadAction{
			From:     desiredApp.DropletUri,
			To:       ".",
			CacheKey: fmt.Sprintf("droplets-%s", lrpGuid),
		})
	}

	action = &models.RunAction{
		Path: "/tmp/circus/soldier",
		Args: append(
			[]string{"/app"},
			desiredApp.StartCommand,
			desiredApp.ExecutionMetadata,
		),
		Env:       createLrpEnv(desiredApp.Environment.BBSEnvironment()),
		LogSource: AppLogSource,
		ResourceLimits: models.ResourceLimits{
			Nofile: numFiles,
		},
	}

	monitor = &models.RunAction{
		Path:      "/tmp/circus/spy",
		Args:      []string{"-addr=:8080"},
		LogSource: HealthLogSource,
	}

	setupAction := models.Serial(setup...)

	return &receptor.DesiredLRPCreateRequest{
		Domain: LRPDomain,

		ProcessGuid: lrpGuid,
		Instances:   desiredApp.NumInstances,
		Routes:      desiredApp.Routes,

		CPUWeight: cpuWeight(desiredApp.MemoryMB),

		MemoryMB: desiredApp.MemoryMB,
		DiskMB:   desiredApp.DiskMB,

		Ports: []uint32{
			8080,
		},

		RootFSPath: rootFSPath,

		Stack: desiredApp.Stack,

		LogGuid:   desiredApp.LogGuid,
		LogSource: LRPLogSource,

		Setup:   setupAction,
		Action:  action,
		Monitor: monitor,
	}, nil
}

func (b RecipeBuilder) circusDownloadURL(circusPath string, fileServerURL string) string {
	staticRoute, ok := SchemaRouter.NewFileServerRoutes().RouteForHandler(SchemaRouter.FS_STATIC)
	if !ok {
		panic("couldn't generate the download path for the bundle of app lifecycle binaries")
	}

	return urljoiner.Join(fileServerURL, staticRoute.Path, circusPath)
}

func createLrpEnv(env []models.EnvironmentVariable) []models.EnvironmentVariable {
	env = append(env, models.EnvironmentVariable{Name: "PORT", Value: "8080"})
	env = append(env, models.EnvironmentVariable{Name: "VCAP_APP_PORT", Value: "8080"})
	env = append(env, models.EnvironmentVariable{Name: "VCAP_APP_HOST", Value: "0.0.0.0"})
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
