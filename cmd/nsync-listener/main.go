package main

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"code.cloudfoundry.org/bbs"
	"code.cloudfoundry.org/cfhttp"
	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/consuladapter"
	"code.cloudfoundry.org/debugserver"
	"code.cloudfoundry.org/diego-ssh/keys"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/locket"
	"code.cloudfoundry.org/nsync/config"
	"code.cloudfoundry.org/nsync/handlers"
	"code.cloudfoundry.org/runtimeschema/cc_messages/flags"
	"github.com/hashicorp/consul/api"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"

	"code.cloudfoundry.org/nsync/recipebuilder"
	"github.com/cloudfoundry/dropsonde"
)

var configPath = flag.String(
	"configPath",
	"",
	"path to config",
)

const (
	dropsondeOrigin = "nsync_listener"
)

func main() {
	flag.Parse()

	logger := lager.NewLogger("nsync-listener")

	listenerConfig, err := config.NewListenerConfig(*configPath)
	if err != nil {
		logger.Fatal(fmt.Sprintf("Couldn't parse config file %s", *configPath), err)
	}
	lifecycles := flags.LifecycleMap{}
	for _, value := range listenerConfig.Lifecycles {
		lifecycles.Set(value)
	}

	reconfigurableSink := newReconfigurableSink(listenerConfig.LagerConfig.LogLevel)
	logger.RegisterSink(reconfigurableSink)
	initializeDropsonde(logger, listenerConfig)
	cfhttp.Initialize(time.Duration(listenerConfig.CommunicationTimeout))

	buildpackRecipeBuilderConfig := recipebuilder.Config{
		Lifecycles:           lifecycles,
		FileServerURL:        listenerConfig.FileServerURL,
		KeyFactory:           keys.RSAKeyPairFactory,
		PrivilegedContainers: listenerConfig.PrivilegedContainers,
	}
	dockerRecipeBuilderConfig := recipebuilder.Config{
		Lifecycles:    lifecycles,
		FileServerURL: listenerConfig.FileServerURL,
		KeyFactory:    keys.RSAKeyPairFactory,
	}

	recipeBuilders := map[string]recipebuilder.RecipeBuilder{
		"buildpack": recipebuilder.NewBuildpackRecipeBuilder(logger, buildpackRecipeBuilderConfig),
		"docker":    recipebuilder.NewDockerRecipeBuilder(logger, dockerRecipeBuilderConfig),
	}

	handler := handlers.New(logger, initializeBBSClient(logger, listenerConfig), recipeBuilders)

	consulClient, err := consuladapter.NewClientFromUrl(listenerConfig.ConsulCluster)
	if err != nil {
		logger.Fatal("new-consul-client-failed", err)
	}

	_, portString, err := net.SplitHostPort(listenerConfig.ListenAddress)
	if err != nil {
		logger.Fatal("failed-invalid-listen-address", err)
	}
	portNum, err := net.LookupPort("tcp", portString)
	if err != nil {
		logger.Fatal("failed-invalid-listen-port", err)
	}

	clock := clock.NewClock()
	registrationRunner := initializeRegistrationRunner(logger, consulClient, portNum, clock)

	members := grouper.Members{
		{"server", http_server.New(listenerConfig.ListenAddress, handler)},
		{"registration-runner", registrationRunner},
	}

	if dbgAddr := listenerConfig.DebugServerConfig.DebugAddress; dbgAddr != "" {
		members = append(grouper.Members{
			{"debug-server", debugserver.Runner(dbgAddr, reconfigurableSink)},
		}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)

	monitor := ifrit.Invoke(sigmon.New(group))

	logger.Info("started")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
}

func initializeDropsonde(logger lager.Logger, listenerConfig config.ListenerConfig) {
	dropsondeDestination := fmt.Sprint("localhost:", listenerConfig.DropsondePort)
	err := dropsonde.Initialize(dropsondeDestination, dropsondeOrigin)
	if err != nil {
		logger.Error("failed to initialize dropsonde: %v", err)
	}
}

func initializeBBSClient(logger lager.Logger, listenerConfig config.ListenerConfig) bbs.Client {
	bbsURL, err := url.Parse(listenerConfig.BBSAddress)
	if err != nil {
		logger.Fatal("Invalid BBS URL", err)
	}

	if bbsURL.Scheme != "https" {
		return bbs.NewClient(listenerConfig.BBSAddress)
	}

	bbsClient, err := bbs.NewSecureClient(
		listenerConfig.BBSAddress,
		listenerConfig.BBSCACert,
		listenerConfig.BBSClientCert,
		listenerConfig.BBSClientKey,
		listenerConfig.BBSClientSessionCacheSize,
		listenerConfig.BBSMaxIdleConnsPerHost,
	)
	if err != nil {
		logger.Fatal("Failed to configure secure BBS client", err)
	}
	return bbsClient
}

func initializeRegistrationRunner(
	logger lager.Logger,
	consulClient consuladapter.Client,
	port int,
	clock clock.Clock) ifrit.Runner {
	registration := &api.AgentServiceRegistration{
		Name: "nsync",
		Port: port,
		Check: &api.AgentServiceCheck{
			TTL: "3s",
		},
	}
	return locket.NewRegistrationRunner(logger, registration, consulClient, locket.RetryInterval, clock)
}

func newReconfigurableSink(logLevel string) *lager.ReconfigurableSink {
	var minLagerLogLevel lager.LogLevel
	switch logLevel {
	case "debug":
		minLagerLogLevel = lager.DEBUG
	case "info":
		minLagerLogLevel = lager.INFO
	case "error":
		minLagerLogLevel = lager.ERROR
	case "fatal":
		minLagerLogLevel = lager.FATAL
	default:
		panic(fmt.Errorf("unknown log level: %s", logLevel))
	}

	return lager.NewReconfigurableSink(lager.NewWriterSink(os.Stdout, lager.DEBUG), minLagerLogLevel)
}
