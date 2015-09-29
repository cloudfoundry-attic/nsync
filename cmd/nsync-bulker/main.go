package main

import (
	"flag"
	"net/url"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/cf-debug-server"
	cf_lager "github.com/cloudfoundry-incubator/cf-lager"
	"github.com/cloudfoundry-incubator/cf_http"
	"github.com/cloudfoundry-incubator/consuladapter"
	"github.com/cloudfoundry-incubator/diego-ssh/keys"
	"github.com/cloudfoundry-incubator/locket"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages/flags"
	"github.com/cloudfoundry/dropsonde"
	"github.com/nu7hatch/gouuid"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"

	"github.com/cloudfoundry-incubator/nsync"
	"github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
)

var bbsAddress = flag.String(
	"bbsAddress",
	"",
	"Address to the BBS Server",
)

var consulCluster = flag.String(
	"consulCluster",
	"",
	"comma-separated list of consul server URLs (scheme://ip:port)",
)

var lockTTL = flag.Duration(
	"lockTTL",
	locket.LockTTL,
	"TTL for service lock",
)

var lockRetryInterval = flag.Duration(
	"lockRetryInterval",
	locket.RetryInterval,
	"interval to wait before retrying a failed lock acquisition",
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

var communicationTimeout = flag.Duration(
	"communicationTimeout",
	30*time.Second,
	"Timeout applied to all HTTP requests.",
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

var fileServerURL = flag.String(
	"fileServerURL",
	"",
	"URL of the file server",
)

var bbsCACert = flag.String(
	"bbsCACert",
	"",
	"path to certificate authority cert used for mutually authenticated TLS BBS communication",
)

var bbsClientCert = flag.String(
	"bbsClientCert",
	"",
	"path to client cert used for mutually authenticated TLS BBS communication",
)

var bbsClientKey = flag.String(
	"bbsClientKey",
	"",
	"path to client key used for mutually authenticated TLS BBS communication",
)

var bbsClientSessionCacheSize = flag.Int(
	"bbsClientSessionCacheSize",
	0,
	"Capacity of the ClientSessionCache option on the TLS configuration. If zero, golang's default will be used",
)

var bbsMaxIdleConnsPerHost = flag.Int(
	"bbsMaxIdleConnsPerHost",
	0,
	"Controls the maximum number of idle (keep-alive) connctions per host. If zero, golang's default will be used",
)

const (
	dropsondeOrigin      = "nsync_bulker"
	dropsondeDestination = "localhost:3457"
)

func main() {
	cf_debug_server.AddFlags(flag.CommandLine)
	cf_lager.AddFlags(flag.CommandLine)

	lifecycles := flags.LifecycleMap{}
	flag.Var(&lifecycles, "lifecycle", "app lifecycle binary bundle mapping (lifecycle[/stack]:bundle-filepath-in-fileserver)")
	flag.Parse()

	cf_http.Initialize(*communicationTimeout)

	logger, reconfigurableSink := cf_lager.New("nsync-bulker")
	initializeDropsonde(logger)

	serviceClient := initializeServiceClient(logger)
	uuid, err := uuid.NewV4()
	if err != nil {
		logger.Fatal("Couldn't generate uuid", err)
	}
	lockMaintainer := serviceClient.NewNsyncBulkerLockRunner(logger, uuid.String(), *lockRetryInterval)

	recipeBuilderConfig := recipebuilder.Config{
		Lifecycles:    lifecycles,
		FileServerURL: *fileServerURL,
		KeyFactory:    keys.RSAKeyPairFactory,
	}
	recipeBuilders := map[string]recipebuilder.RecipeBuilder{
		"buildpack": recipebuilder.NewBuildpackRecipeBuilder(logger, recipeBuilderConfig),
		"docker":    recipebuilder.NewDockerRecipeBuilder(logger, recipeBuilderConfig),
	}

	runner := bulk.NewProcessor(
		initializeBBSClient(logger),
		*pollingInterval,
		*domainTTL,
		*bulkBatchSize,
		*skipCertVerify,
		logger,
		&bulk.CCFetcher{
			BaseURI:   *ccBaseURL,
			BatchSize: int(*bulkBatchSize),
			Username:  *ccUsername,
			Password:  *ccPassword,
		},
		recipeBuilders,
		clock.NewClock(),
	)

	members := grouper.Members{
		{"lock-maintainer", lockMaintainer},
		{"runner", runner},
	}

	if dbgAddr := cf_debug_server.DebugAddress(flag.CommandLine); dbgAddr != "" {
		members = append(grouper.Members{
			{"debug-server", cf_debug_server.Runner(dbgAddr, reconfigurableSink)},
		}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)

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
	err := dropsonde.Initialize(dropsondeDestination, dropsondeOrigin)
	if err != nil {
		logger.Error("failed to initialize dropsonde: %v", err)
	}
}

func initializeServiceClient(logger lager.Logger) nsync.ServiceClient {
	client, err := consuladapter.NewClient(*consulCluster)
	if err != nil {
		logger.Fatal("new-client-failed", err)
	}

	sessionMgr := consuladapter.NewSessionManager(client)
	consulSession, err := consuladapter.NewSession("nsync-bulker", *lockTTL, client, sessionMgr)
	if err != nil {
		logger.Fatal("consul-session-failed", err)
	}

	return nsync.NewServiceClient(consulSession, clock.NewClock())
}

func initializeBBSClient(logger lager.Logger) bbs.Client {
	bbsURL, err := url.Parse(*bbsAddress)
	if err != nil {
		logger.Fatal("Invalid BBS URL", err)
	}

	if bbsURL.Scheme != "https" {
		return bbs.NewClient(*bbsAddress)
	}

	bbsClient, err := bbs.NewSecureClient(*bbsAddress, *bbsCACert, *bbsClientCert, *bbsClientKey, *bbsClientSessionCacheSize, *bbsMaxIdleConnsPerHost)
	if err != nil {
		logger.Fatal("Failed to configure secure BBS client", err)
	}
	return bbsClient
}
