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
	"github.com/cloudfoundry-incubator/receptor"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/lock_bbs"
	"github.com/cloudfoundry/dropsonde"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/gunk/workpool"
	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"
	"github.com/nu7hatch/gouuid"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"

	"github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
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

var heartbeatInterval = flag.Duration(
	"heartbeatInterval",
	lock_bbs.HEARTBEAT_INTERVAL,
	"the interval between heartbeats to the lock",
)

var ccBaseURL = flag.String(
	"ccBaseURL",
	"",
	"base URL of the cloud controller",
)

var ccUsername = flag.String(
	"ccUsername",
	"",
	"basic auth username for CC bulk API",
)

var ccPassword = flag.String(
	"ccPassword",
	"",
	"basic auth password for CC bulk API",
)

var ccFetchTimeout = flag.Duration(
	"ccFetchTimeout",
	30*time.Second,
	"how long to wait for bulk app request to CC to respond",
)

var pollingInterval = flag.Duration(
	"pollingInterval",
	30*time.Second,
	"interval at which to poll bulk API",
)

var domainTTL = flag.Duration(
	"domainTTL",
	2*time.Minute,
	"duration of the domain; bumped on every bulk sync",
)

var bulkBatchSize = flag.Uint(
	"bulkBatchSize",
	500,
	"number of apps to fetch at once from bulk API",
)

var skipCertVerify = flag.Bool(
	"skipCertVerify",
	false,
	"skip SSL certificate verification",
)

var circuses = flag.String(
	"circuses",
	"",
	"app lifecycle binary bundle mapping (stack => bundle filename in fileserver)",
)

var dockerCircusPath = flag.String(
	"dockerCircusPath",
	"",
	"path for downloading docker circus from file server",
)

var fileServerURL = flag.String(
	"fileServerURL",
	"",
	"URL of the file server",
)

var dropsondeOrigin = flag.String(
	"dropsondeOrigin",
	"nsync_bulker",
	"Origin identifier for dropsonde-emitted metrics.",
)

var dropsondeDestination = flag.String(
	"dropsondeDestination",
	"localhost:3457",
	"Destination for dropsonde-emitted metrics.",
)

func main() {
	flag.Parse()

	logger := cf_lager.New("nsync-bulker")
	initializeDropsonde(logger)

	diegoAPIClient := receptor.NewClient(*diegoAPIURL)
	bbs := initializeBbs(logger)

	cf_debug_server.Run()

	uuid, err := uuid.NewV4()
	if err != nil {
		logger.Fatal("Couldn't generate uuid", err)
	}

	var circuseDownloadURLs map[string]string
	err = json.Unmarshal([]byte(*circuses), &circuseDownloadURLs)
	if err != nil {
		logger.Fatal("invalid-circus-mapping", err)
	}

	if *dockerCircusPath == "" {
		logger.Fatal("empty-docker-circus-path", errors.New("dockerCircusPath flag not provided"))
	}

	recipeBuilder := recipebuilder.New(circuseDownloadURLs, *dockerCircusPath, *fileServerURL, logger)

	heartbeater := bbs.NewNsyncBulkerLock(uuid.String(), *heartbeatInterval)

	runner := bulk.NewProcessor(
		diegoAPIClient,
		*pollingInterval,
		*ccFetchTimeout,
		*domainTTL,
		*bulkBatchSize,
		*skipCertVerify,
		logger,
		&bulk.CCFetcher{
			BaseURI:   *ccBaseURL,
			BatchSize: *bulkBatchSize,
			Username:  *ccUsername,
			Password:  *ccPassword,
		},
		bulk.NewDiffer(recipeBuilder, logger),
		timeprovider.NewTimeProvider(),
	)

	group := grouper.NewOrdered(os.Interrupt, grouper.Members{
		{"heartbeater", heartbeater},
		{"runner", runner},
	})

	logger.Info("waiting-for-lock")

	monitor := ifrit.Invoke(sigmon.New(group))

	logger.Info("started")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
	os.Exit(0)
}

func initializeDropsonde(logger lager.Logger) {
	err := dropsonde.Initialize(*dropsondeDestination, *dropsondeOrigin)
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

	return Bbs.NewNsyncBBS(etcdAdapter, timeprovider.NewTimeProvider(), logger)
}
