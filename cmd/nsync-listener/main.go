package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/cf-debug-server"
	"github.com/cloudfoundry-incubator/cf-lager"
	"github.com/cloudfoundry-incubator/cf_http"
	"github.com/cloudfoundry-incubator/receptor"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/lock_bbs"
	"github.com/cloudfoundry/gunk/diegonats"
	"github.com/cloudfoundry/gunk/workpool"
	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"
	"github.com/nu7hatch/gouuid"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"

	"github.com/cloudfoundry-incubator/nsync/listen"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry/dropsonde"
)

var heartbeatInterval = flag.Duration(
	"heartbeatInterval",
	lock_bbs.HEARTBEAT_INTERVAL,
	"the interval between heartbeats to the lock",
)

var etcdCluster = flag.String(
	"etcdCluster",
	"http://127.0.0.1:4001",
	"comma-separated list of etcd addresses (http://ip:port)",
)

var diegoAPIURL = flag.String(
	"diegoAPIURL",
	"",
	"URL of diego API",
)

var natsAddresses = flag.String(
	"natsAddresses",
	"127.0.0.1:4222",
	"comma-separated list of NATS addresses (ip:port)",
)

var natsUsername = flag.String(
	"natsUsername",
	"nats",
	"Username to connect to nats",
)

var natsPassword = flag.String(
	"natsPassword",
	"nats",
	"Password for nats user",
)

var lifecycles = flag.String(
	"lifecycles",
	"",
	"app lifecycle binary bundle mapping (stack => bundle filename in fileserver)",
)

var dockerLifecyclePath = flag.String(
	"dockerLifecyclePath",
	"",
	"path for downloading docker lifecycle from file server",
)

var fileServerURL = flag.String(
	"fileServerURL",
	"",
	"URL of the file server",
)

var communicationTimeout = flag.Duration(
	"communicationTimeout",
	30*time.Second,
	"Timeout applied to all HTTP requests.",
)

const (
	dropsondeOrigin      = "nsync_listener"
	dropsondeDestination = "localhost:3457"
)

func main() {
	cf_debug_server.AddFlags(flag.CommandLine)
	cf_lager.AddFlags(flag.CommandLine)
	flag.Parse()

	cf_http.Initialize(*communicationTimeout)
	logger, reconfigurableSink := cf_lager.New("nsync-listener")

	initializeDropsonde(logger)

	diegoAPIClient := receptor.NewClient(*diegoAPIURL)
	bbs := initializeBbs(logger)

	var lifecycleDownloadURLs map[string]string
	err := json.Unmarshal([]byte(*lifecycles), &lifecycleDownloadURLs)

	if *dockerLifecyclePath == "" {
		logger.Fatal("empty-docker_app_lifecycle-path", errors.New("dockerLifecyclePath flag not provided"))
	}

	if err != nil {
		logger.Fatal("invalid-lifecycle-mapping", err)
	}

	recipeBuilder := recipebuilder.New(lifecycleDownloadURLs, *dockerLifecyclePath, *fileServerURL, logger)

	uuid, err := uuid.NewV4()
	if err != nil {
		logger.Fatal("Couldn't generate uuid", err)
	}

	nsyncLock := bbs.NewNsyncListenerLock(uuid.String(), *heartbeatInterval)
	natsClient := diegonats.NewClient()
	natsClientRunner := diegonats.NewClientRunner(*natsAddresses, *natsUsername, *natsPassword, logger, natsClient)
	listener := listen.Listen{
		NATSClient:     natsClient,
		ReceptorClient: diegoAPIClient,
		Logger:         logger,
		RecipeBuilder:  recipeBuilder,
	}

	members := grouper.Members{
		{"nsyncLock", nsyncLock},
		{"nats-client", natsClientRunner},
		{"listener", listener},
	}

	if dbgAddr := cf_debug_server.DebugAddress(flag.CommandLine); dbgAddr != "" {
		members = append(grouper.Members{
			{"debug-server", cf_debug_server.Runner(dbgAddr, reconfigurableSink)},
		}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)

	logger.Info("waiting-for-lock")

	monitor := ifrit.Envoke(sigmon.New(group))

	logger.Info("started")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
}

func initializeDropsonde(logger lager.Logger) {
	err := dropsonde.Initialize(dropsondeDestination, dropsondeOrigin)
	if err != nil {
		logger.Error("failed to initialize dropsonde: %v", err)
	}
}

func initializeBbs(logger lager.Logger) Bbs.NsyncBBS {
	etcdAdapter := etcdstoreadapter.NewETCDStoreAdapter(
		strings.Split(*etcdCluster, ","),
		workpool.NewWorkPool(10),
	)

	err := etcdAdapter.Connect()
	if err != nil {
		logger.Fatal("failed-to-connect-to-etcd", err)
	}

	return Bbs.NewNsyncBBS(etcdAdapter, clock.NewClock(), logger)
}
