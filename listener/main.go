package main

import (
	"flag"
	"os"
	"strings"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"
	"github.com/cloudfoundry/storeadapter/workerpool"
	"github.com/cloudfoundry/yagnats"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"

	"github.com/cloudfoundry-incubator/nsync/listen"
)

var etcdCluster = flag.String(
	"etcdCluster",
	"http://127.0.0.1:4001",
	"comma-separated list of etcd addresses (http://ip:port)",
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

var syslogName = flag.String(
	"syslogName",
	"",
	"syslog name",
)

func main() {
	flag.Parse()

	logger := initializeLogger()
	natsClient := initializeNatsClient(logger)
	bbs := initializeBbs(logger)

	group := grouper.EnvokeGroup(grouper.RunGroup{
		"listener": listen.Listen{
			NATSClient: natsClient,
			BBS:        bbs,
			Logger:     logger,
		},
	})

	logger.Info("nsync.listener.started")

	monitor := ifrit.Envoke(sigmon.New(group))

	err := <-monitor.Wait()
	if err != nil {
		logger.Errord(map[string]interface{}{
			"error": err.Error(),
		}, "nsync.listener.exited")
		os.Exit(1)
	}

	logger.Info("nsync.listener.exited")
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

	return steno.NewLogger("Listener")
}

func initializeNatsClient(logger *steno.Logger) yagnats.NATSClient {
	natsClient := yagnats.NewClient()

	natsMembers := []yagnats.ConnectionProvider{}
	for _, addr := range strings.Split(*natsAddresses, ",") {
		natsMembers = append(
			natsMembers,
			&yagnats.ConnectionInfo{addr, *natsUsername, *natsPassword},
		)
	}

	err := natsClient.Connect(&yagnats.ConnectionCluster{
		Members: natsMembers,
	})

	if err != nil {
		logger.Fatalf("Error connecting to NATS: %s\n", err)
	}

	return natsClient
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
