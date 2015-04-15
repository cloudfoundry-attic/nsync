package recipebuilder

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry-incubator/runtime-schema/routes"
	"github.com/cloudfoundry/gunk/urljoiner"
	"github.com/pivotal-golang/lager"
)

const (
	DockerScheme      = "docker"
	DockerIndexServer = "docker.io"

	MinCpuProxy = 256
	MaxCpuProxy = 8192

	DefaultFileDescriptorLimit = uint64(1024)

	LRPLogSource    = "CELL"
	AppLogSource    = "APP"
	HealthLogSource = "HEALTH"

	Router      = "router"
	DefaultPort = uint16(8080)

	DefaultLANG = "en_US.UTF-8"
)

var (
	ErrNoLifecycleDefined = errors.New("no lifecycle binary bundle defined for stack")
	ErrAppSourceMissing   = errors.New("desired app missing both droplet_uri and docker_image; exactly one is required.")
	ErrMultipleAppSources = errors.New("desired app contains both droplet_uri and docker_image; exactly one is required.")
)

type RecipeBuilder struct {
	logger        lager.Logger
	lifecycles    map[string]string
	fileServerURL string
}

func New(lifecycles map[string]string, fileServerURL string, logger lager.Logger) *RecipeBuilder {
	return &RecipeBuilder{
		lifecycles:    lifecycles,
		logger:        logger,
		fileServerURL: fileServerURL,
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

	isDocker := desiredApp.DockerImageUrl != ""

	var lifecycle string
	if isDocker {
		lifecycle = "docker"
	} else {
		lifecycle = "buildpack/" + desiredApp.Stack
	}

	lifecyclePath, ok := b.lifecycles[lifecycle]
	if !ok {
		buildLogger.Error("unknown-lifecycle", ErrNoLifecycleDefined, lager.Data{
			"lifecycle": lifecycle,
		})

		return nil, ErrNoLifecycleDefined
	}

	lifecycleURL := b.lifecycleDownloadURL(lifecyclePath, b.fileServerURL)

	rootFSPath := ""
	if isDocker {
		var err error
		rootFSPath, err = convertDockerURI(desiredApp.DockerImageUrl)
		if err != nil {
			return nil, err
		}
	} else {
		rootFSPath = models.PreloadedRootFS(desiredApp.Stack)
	}

	var privilegedContainer bool
	var containerEnvVars []receptor.EnvironmentVariable

	if !isDocker {
		privilegedContainer = true
		containerEnvVars = append(containerEnvVars, receptor.EnvironmentVariable{"LANG", DefaultLANG})
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

	if !isDocker {
		setup = append(setup, &models.DownloadAction{
			From:     desiredApp.DropletUri,
			To:       ".",
			CacheKey: fmt.Sprintf("droplets-%s", lrpGuid),
		})
	}

	action = &models.RunAction{
		Path: "/tmp/lifecycle/launcher",
		Args: append(
			[]string{"app"},
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

	desiredAppRoutingInfo := cfroutes.CFRoutes{
		{Hostnames: desiredApp.Routes, Port: DefaultPort},
	}.RoutingInfo()

	return &receptor.DesiredLRPCreateRequest{
		Privileged: privilegedContainer,

		Domain: cc_messages.AppLRPDomain,

		ProcessGuid: lrpGuid,
		Instances:   desiredApp.NumInstances,
		Routes:      desiredAppRoutingInfo,
		Annotation:  desiredApp.ETag,

		CPUWeight: cpuWeight(desiredApp.MemoryMB),

		MemoryMB: desiredApp.MemoryMB,
		DiskMB:   desiredApp.DiskMB,

		Ports: []uint16{
			DefaultPort,
		},

		RootFS: rootFSPath,

		LogGuid:   desiredApp.LogGuid,
		LogSource: LRPLogSource,

		MetricsGuid: desiredApp.LogGuid,

		EnvironmentVariables: containerEnvVars,
		Setup:                setupAction,
		Action:               action,
		Monitor:              monitor,

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

func convertDockerURI(dockerURI string) (string, error) {
	if strings.Contains(dockerURI, "://") {
		return "", errors.New("docker URI [" + dockerURI + "] should not contain scheme")
	}

	indexName, remoteName, tag := parseDockerRepoUrl(dockerURI)

	return (&url.URL{
		Scheme:   DockerScheme,
		Path:     indexName + "/" + remoteName,
		Fragment: tag,
	}).String(), nil
}

// via https://github.com/docker/docker/blob/a271eaeba224652e3a12af0287afbae6f82a9333/registry/config.go#L295
func parseDockerRepoUrl(dockerURI string) (indexName, remoteName, tag string) {
	nameParts := strings.SplitN(dockerURI, "/", 2)

	if officialRegistry(nameParts) {
		// URI without host
		indexName = ""
		remoteName = dockerURI

		// URI has format docker.io/<path>
		if nameParts[0] == DockerIndexServer {
			indexName = DockerIndexServer
			remoteName = nameParts[1]
		}

		// Remote name contain no '/' - prefix it with "library/"
		// via https://github.com/docker/docker/blob/a271eaeba224652e3a12af0287afbae6f82a9333/registry/config.go#L343
		if strings.IndexRune(remoteName, '/') == -1 {
			remoteName = "library/" + remoteName
		}
	} else {
		indexName = nameParts[0]
		remoteName = nameParts[1]
	}

	remoteName, tag = parseDockerRepositoryTag(remoteName)

	return indexName, remoteName, tag
}

func officialRegistry(nameParts []string) bool {
	return len(nameParts) == 1 ||
		nameParts[0] == DockerIndexServer ||
		(!strings.Contains(nameParts[0], ".") &&
			!strings.Contains(nameParts[0], ":") &&
			nameParts[0] != "localhost")
}

// via https://github.com/docker/docker/blob/4398108/pkg/parsers/parsers.go#L72
func parseDockerRepositoryTag(remoteName string) (string, string) {
	n := strings.LastIndex(remoteName, ":")
	if n < 0 {
		return remoteName, ""
	}
	if tag := remoteName[n+1:]; !strings.Contains(tag, "/") {
		return remoteName[:n], tag
	}
	return remoteName, ""
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
