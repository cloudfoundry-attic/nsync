package listen

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/apcera/nats"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gunk/diegonats"
	"github.com/cloudfoundry/storeadapter"
	"github.com/pivotal-golang/lager"
)

const (
	DesireAppTopic       = "diego.desire.app"
	DesireDockerAppTopic = "diego.docker.desire.app"
	KillIndexTopic       = "diego.kill.index"

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
	Build(cc_messages.DesireAppRequestFromCC) (models.DesiredLRP, error)
}

type Listen struct {
	RecipeBuilder RecipeBuilder
	NATSClient    diegonats.NATSClient
	BBS           Bbs.NsyncBBS
	Logger        lager.Logger
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
				listen.desireApp(msg)
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
	err := listen.BBS.RequestStopLRPIndex(msg.ProcessGuid, msg.Index)
	if err != nil {
		listen.Logger.Error("request-stop-index-failed", err)
		return
	}
	listen.Logger.Debug("requested-stop-index", lager.Data{
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

func (listen Listen) desireApp(desireAppMessage cc_messages.DesireAppRequestFromCC) {
	requestLogger := listen.Logger.Session("desire-lrp", lager.Data{
		"desired-app-message": desireAppMessage,
	})

	if desireAppMessage.NumInstances == 0 {
		err := listen.BBS.RemoveDesiredLRPByProcessGuid(desireAppMessage.ProcessGuid)
		if err == storeadapter.ErrorKeyNotFound {
			requestLogger.Info("lrp-already-deleted")
			return
		}
		if err != nil {
			requestLogger.Error("remove-failed", err)
			return
		}
	} else {
		desiredLRP, err := listen.RecipeBuilder.Build(desireAppMessage)
		if err != nil {
			requestLogger.Error("failed-to-build-recipe", err)
			return
		}

		desiredLRPCounter.Increment()
		err = listen.BBS.DesireLRP(desiredLRP)
		if err != nil {
			requestLogger.Error("failed", err)
			return
		}
	}
}
