package listen

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/apcera/nats"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/cloudfoundry/gunk/diegonats"
	"github.com/pivotal-golang/lager"
)

const (
	DesireAppTopic       = "diego.desire.app"
	DesireDockerAppTopic = "diego.docker.desire.app"
	KillIndexTopic       = "diego.stop.index"

	desiredLRPCounter = metric.Counter("LRPsDesired")
)

type desireAppChan chan cc_messages.DesireAppRequestFromCC

func (desiredApps desireAppChan) sendDesireAppRequest(message *nats.Msg, logger lager.Logger) {
	desireAppMessage := cc_messages.DesireAppRequestFromCC{}
	err := json.Unmarshal(message.Data, &desireAppMessage)
	if err != nil {
		logger.Error("parse-nats-message-failed", err)
		return
	}

	desiredApps <- desireAppMessage
}

type RecipeBuilder interface {
	Build(*cc_messages.DesireAppRequestFromCC) (*receptor.DesiredLRPCreateRequest, error)
}

type Listen struct {
	RecipeBuilder  RecipeBuilder
	NATSClient     diegonats.NATSClient
	ReceptorClient receptor.Client
	Logger         lager.Logger
}

func (listen Listen) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	wg := new(sync.WaitGroup)
	desiredApps := make(desireAppChan)
	killIndexChan := make(chan cc_messages.KillIndexRequestFromCC)

	desiredAppsSub, err := listen.listenForDesiredApps(desiredApps)
	if err != nil {
		return err
	}
	defer desiredAppsSub.Unsubscribe()

	desiredDockerSub, err := listen.listenForDesiredDockerApps(desiredApps)
	if err != nil {
		return err
	}
	defer desiredDockerSub.Unsubscribe()

	killIndexSub, err := listen.listenForKillIndex(killIndexChan)
	if err != nil {
		return err
	}
	defer killIndexSub.Unsubscribe()

	close(ready)

	for {
		select {
		case msg := <-desiredApps:
			wg.Add(1)
			go func() {
				defer wg.Done()
				listen.processDesireAppRequest(msg)
			}()

		case msg := <-killIndexChan:
			wg.Add(1)
			go func() {
				defer wg.Done()
				listen.killIndex(msg)
			}()

		case <-signals:
			wg.Wait()
			return nil
		}
	}
}

func (listen Listen) killIndex(msg cc_messages.KillIndexRequestFromCC) {
	err := listen.ReceptorClient.KillActualLRPByProcessGuidAndIndex(msg.ProcessGuid, msg.Index)
	if err != nil {
		listen.Logger.Error("request-stop-index-failed", err)
		return
	}
	listen.Logger.Info("requested-stop-index", lager.Data{
		"process_guid": msg.ProcessGuid,
		"index":        msg.Index,
	})
}

func (listen Listen) listenForKillIndex(killIndexChan chan cc_messages.KillIndexRequestFromCC) (*nats.Subscription, error) {
	return listen.NATSClient.Subscribe(KillIndexTopic, func(message *nats.Msg) {
		killIndexReq := cc_messages.KillIndexRequestFromCC{}
		err := json.Unmarshal(message.Data, &killIndexReq)
		if err != nil {
			listen.Logger.Error("unmarshal-kill-index-request-failed", err)
			return
		}
		killIndexChan <- killIndexReq
	})
}

func (listen Listen) listenForDesiredApps(desiredApps desireAppChan) (*nats.Subscription, error) {
	return listen.NATSClient.Subscribe(DesireAppTopic, func(message *nats.Msg) {
		desiredApps.sendDesireAppRequest(message, listen.Logger)
	})
}

func (listen Listen) listenForDesiredDockerApps(desiredApps desireAppChan) (*nats.Subscription, error) {
	return listen.NATSClient.Subscribe(DesireDockerAppTopic, func(message *nats.Msg) {
		desiredApps.sendDesireAppRequest(message, listen.Logger)
	})
}

func (listen Listen) processDesireAppRequest(desireAppMessage cc_messages.DesireAppRequestFromCC) {
	requestLogger := listen.Logger.Session("desire-lrp", lager.Data{
		"desired-app-message": desireAppMessage,
	})

	if desireAppMessage.NumInstances == 0 {
		listen.deleteDesiredApp(requestLogger, desireAppMessage.ProcessGuid)
		return
	}

	desiredAppExists, err := listen.desiredAppExists(requestLogger, desireAppMessage.ProcessGuid)
	if err != nil {
		return
	}

	desiredLRPCounter.Increment()

	if desiredAppExists {
		listen.updateDesiredApp(requestLogger, desireAppMessage)
	} else {
		listen.createDesiredApp(requestLogger, desireAppMessage)
	}
}

func (listen Listen) desiredAppExists(logger lager.Logger, processGuid string) (bool, error) {
	_, err := listen.ReceptorClient.GetDesiredLRP(processGuid)
	if err == nil {
		return true, nil
	}

	if rerr, ok := err.(receptor.Error); ok {
		if rerr.Type == receptor.DesiredLRPNotFound {
			return false, nil
		}
	}

	logger.Error("unexpected-error-from-get-desired-lrp", err)
	return false, err
}

func (listen Listen) createDesiredApp(logger lager.Logger, desireAppMessage cc_messages.DesireAppRequestFromCC) {
	desiredLRP, err := listen.RecipeBuilder.Build(&desireAppMessage)
	if err != nil {
		logger.Error("failed-to-build-recipe", err)
		return
	}

	err = listen.ReceptorClient.CreateDesiredLRP(*desiredLRP)
	if err != nil {
		logger.Error("failed-to-create", err)
	}
}

func (listen Listen) updateDesiredApp(logger lager.Logger, desireAppMessage cc_messages.DesireAppRequestFromCC) {

	desiredAppRoutes := receptor.RoutingInfo{
		CFRoutes: []receptor.CFRoute{
			{
				Port:      recipebuilder.DefaultPort,
				Hostnames: desireAppMessage.Hostnames,
			},
		},
	}

	updateRequest := receptor.DesiredLRPUpdateRequest{
		Annotation: &desireAppMessage.ETag,
		Instances:  &desireAppMessage.NumInstances,
		Routes:     &desiredAppRoutes,
	}

	err := listen.ReceptorClient.UpdateDesiredLRP(desireAppMessage.ProcessGuid, updateRequest)
	if err != nil {
		logger.Error("failed-to-update-lrp", err)
	}
}

func (listen Listen) deleteDesiredApp(logger lager.Logger, processGuid string) {
	err := listen.ReceptorClient.DeleteDesiredLRP(processGuid)
	if err == nil {
		return
	}

	if rerr, ok := err.(receptor.Error); ok {
		if rerr.Type == receptor.DesiredLRPNotFound {
			logger.Info("lrp-already-deleted")
			return
		}
	}

	logger.Error("failed-to-remove", err)
}
