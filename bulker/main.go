package main

import (
	"flag"
	"os"
	"strings"
	"time"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"
	"github.com/cloudfoundry/storeadapter/workerpool"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"

	"github.com/cloudfoundry-incubator/nsync/bulk"
)

var etcdCluster = flag.String(
	"etcdCluster",
	"http://127.0.0.1:4001",
	"comma-separated list of etcd addresses (http://ip:port)",
)

var syslogName = flag.String(
	"syslogName",
	"",
	"syslog name",
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

var pollingInterval = flag.Duration(
	"pollingInterval",
	30*time.Second,
	"interval at which to poll bulk API",
)

var bulkBatchSize = flag.Uint(
	"bulkBatchSize",
	500,
	"number of apps to fetch at once from bulk API",
)

func main() {
	flag.Parse()

	logger := initializeLogger()
	bbs := initializeBbs(logger)

	group := grouper.EnvokeGroup(grouper.RunGroup{
		"bulk": bulk.NewProcessor(bbs, *pollingInterval, *bulkBatchSize, logger, &bulk.CCFetcher{
			BaseURI:   *ccBaseURL,
			BatchSize: *bulkBatchSize,
			Username:  *ccUsername,
			Password:  *ccPassword,
		}),
	})

	logger.Info("nsync.bulker.started")

	monitor := ifrit.Envoke(sigmon.New(group))

	err := <-monitor.Wait()
	if err != nil {
		logger.Errord(map[string]interface{}{
			"error": err.Error(),
		}, "nsync.bulker.exited")
		os.Exit(1)
	}

	logger.Info("nsync.bulker.exited")
}

func initializeLogger() *steno.Logger {
	stenoConfig := &steno.Config{
		Sinks: []steno.Sink{
			steno.NewIOSink(os.Stdout),
		},
	}

	if *syslogName != "" {
		stenoConfig.Sinks = append(stenoConfig.Sinks, steno.NewSyslogSink(*syslogName))
	}

	steno.Init(stenoConfig)

	return steno.NewLogger("Bulker")
}

func initializeBbs(logger *steno.Logger) Bbs.NsyncBBS {
	etcdAdapter := etcdstoreadapter.NewETCDStoreAdapter(
		strings.Split(*etcdCluster, ","),
		workerpool.NewWorkerPool(10),
	)

	err := etcdAdapter.Connect()
	if err != nil {
		logger.Fatalf("Error connecting to etcd: %s\n", err)
	}

	return Bbs.NewNsyncBBS(etcdAdapter, timeprovider.NewTimeProvider(), logger)
}
