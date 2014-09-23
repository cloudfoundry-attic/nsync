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
	"github.com/cloudfoundry/yagnats"
	"github.com/pivotal-golang/lager"
)

const (
	DesireAppTopic       = "diego.desire.app"
	DesireDockerAppTopic = "diego.docker.desire.app"
	desiredLRPCounter    = metric.Counter("LRPsDesired")
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
	NATSClient    yagnats.ApceraWrapperNATSClient
	BBS           Bbs.NsyncBBS
	Logger        lager.Logger
}

func (listen Listen) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	wg := new(sync.WaitGroup)
	desiredApps := make(chan cc_messages.DesireAppRequestFromCC)

	var desiredAppsSub, desiredDockerSub *nats.Subscription
	defer func() {
		if desiredAppsSub != nil {
			desiredAppsSub.Unsubscribe()
		}
		if desiredDockerSub != nil {
			desiredDockerSub.Unsubscribe()
		}
	}()

	var err error
	desiredAppsSub, err = listen.listenForDesiredApps(desiredApps)
	if err != nil {
		return err
	}
	desiredDockerSub, err = listen.listenForDesiredDockerApps(desiredApps)
	if err != nil {
		return err
	}

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
			wg.Wait()
			return nil
		}
	}
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
