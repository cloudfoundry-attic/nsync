package listen

import (
	"encoding/json"
	"os"
	"sync"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/yagnats"
	"github.com/pivotal-golang/lager"
)

const (
	DesireAppTopic       = "diego.desire.app"
	DesireDockerAppTopic = "diego.docker.desire.app"
	desiredLRPCounter    = metric.Counter("desired-lrp")
)

type desireAppChan chan cc_messages.DesireAppRequestFromCC

func (desiredApps desireAppChan) sendDesireAppRequest(message *yagnats.Message, logger lager.Logger) {
	desireAppMessage := cc_messages.DesireAppRequestFromCC{}
	err := json.Unmarshal(message.Payload, &desireAppMessage)
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
	NATSClient    yagnats.NATSClient
	BBS           Bbs.NsyncBBS
	Logger        lager.Logger
}

func (listen Listen) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	wg := new(sync.WaitGroup)
	desiredApps := make(chan cc_messages.DesireAppRequestFromCC)

	listen.listenForDesiredApps(desiredApps)
	listen.listenForDesiredDockerApps(desiredApps)

	close(ready)

	for {
		select {
		case msg := <-desiredApps:
			wg.Add(1)
			go func() {
				defer wg.Done()
				listen.desireApp(msg)
			}()
		case <-signals:
			listen.NATSClient.UnsubscribeAll(DesireAppTopic)
			wg.Wait()
			return nil
		}
	}
}

func (listen Listen) listenForDesiredApps(desiredApps desireAppChan) {
	listen.NATSClient.Subscribe(DesireAppTopic, func(message *yagnats.Message) {
		desiredApps.sendDesireAppRequest(message, listen.Logger)
	})
}

func (listen Listen) listenForDesiredDockerApps(desiredApps desireAppChan) {
	listen.NATSClient.Subscribe(DesireDockerAppTopic, func(message *yagnats.Message) {
		desiredApps.sendDesireAppRequest(message, listen.Logger)
	})
}

func (listen Listen) desireApp(desireAppMessage cc_messages.DesireAppRequestFromCC) {
	requestLogger := listen.Logger.Session("desire-lrp", lager.Data{
		"desired-app-message": desireAppMessage,
	})

	if desireAppMessage.NumInstances == 0 {
		err := listen.BBS.RemoveDesiredLRPByProcessGuid(desireAppMessage.ProcessGuid)
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
