package main

import (
	"flag"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/cf-debug-server"
	cf_lager "github.com/cloudfoundry-incubator/cf-lager"
	"github.com/cloudfoundry-incubator/cf_http"
	"github.com/cloudfoundry-incubator/nsync/handlers"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages/flags"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"

	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry/dropsonde"
)

var diegoAPIURL = flag.String(
	"diegoAPIURL",
	"",
	"URL of diego API",
)

var nsyncURL = flag.String(
	"nsyncURL",
	"",
	"URL of nsync",
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

	lifecycles := flags.LifecycleMap{}
	flag.Var(&lifecycles, "lifecycle", "app lifecycle binary bundle mapping (lifecycle[/stack]:bundle-filepath-in-fileserver)")
	flag.Parse()

	cf_http.Initialize(*communicationTimeout)
	logger, reconfigurableSink := cf_lager.New("nsync-listener")

	initializeDropsonde(logger)

	diegoAPIClient := receptor.NewClient(*diegoAPIURL)

	recipeBuilder := recipebuilder.New(lifecycles, *fileServerURL, logger)

	handler := handlers.New(logger, diegoAPIClient, recipeBuilder)

	address, err := getNsyncListenerAddress()
	if err != nil {
		logger.Fatal("Invalid nsync listener URL", err)
	}

	members := grouper.Members{
		{"server", http_server.New(address, handler)},
	}

	if dbgAddr := cf_debug_server.DebugAddress(flag.CommandLine); dbgAddr != "" {
		members = append(grouper.Members{
			{"debug-server", cf_debug_server.Runner(dbgAddr, reconfigurableSink)},
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

func initializeDropsonde(logger lager.Logger) {
	err := dropsonde.Initialize(dropsondeDestination, dropsondeOrigin)
	if err != nil {
		logger.Error("failed to initialize dropsonde: %v", err)
	}
}

func getNsyncListenerAddress() (string, error) {
	url, err := url.Parse(*nsyncURL)
	if err != nil {
		return "", err
	}

	_, port, err := net.SplitHostPort(url.Host)
	if err != nil {
		return "", err
	}

	return "0.0.0.0:" + port, nil
}
