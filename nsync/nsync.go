package nsync

import (
	"encoding/json"
	"os"
	"sync"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/yagnats"
)

const DesireAppTopic = "diego.desire.app"

type Nsync struct {
	natsClient yagnats.NATSClient
	bbs        Bbs.AppManagerBBS
	logger     *steno.Logger
}

func NewNsync(
	natsClient yagnats.NATSClient,
	bbs Bbs.AppManagerBBS,
	logger *steno.Logger,
) *Nsync {
	return &Nsync{
		natsClient: natsClient,
		bbs:        bbs,
		logger:     logger,
	}
}

func (n *Nsync) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	wg := new(sync.WaitGroup)
	desiredApps := make(chan models.DesireAppRequestFromCC)
	n.listenForDesiredApps(desiredApps)

	close(ready)

	for {
		select {
		case msg := <-desiredApps:
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.desireApp(msg)
			}()
		case <-signals:
			n.natsClient.UnsubscribeAll(DesireAppTopic)
			wg.Wait()
			return nil
		}
	}
}

func (n *Nsync) listenForDesiredApps(desiredApps chan models.DesireAppRequestFromCC) {
	n.natsClient.Subscribe(DesireAppTopic, func(message *yagnats.Message) {
		desireAppMessage := models.DesireAppRequestFromCC{}
		err := json.Unmarshal(message.Payload, &desireAppMessage)
		if err != nil {
			n.logger.Errorf("Failed to parse NATS message.")
			return
		}

		desiredApps <- desireAppMessage
	})
}

func (n *Nsync) desireApp(desireAppMessage models.DesireAppRequestFromCC) {
	desiredLRP := models.DesiredLRP{
		ProcessGuid:     desireAppMessage.ProcessGuid,
		Source:          desireAppMessage.DropletUri,
		FileDescriptors: desireAppMessage.FileDescriptors,
		Environment:     desireAppMessage.Environment,
		StartCommand:    desireAppMessage.StartCommand,
		Instances:       desireAppMessage.NumInstances,
		MemoryMB:        desireAppMessage.MemoryMB,
		DiskMB:          desireAppMessage.DiskMB,
		Stack:           desireAppMessage.Stack,
		Routes:          desireAppMessage.Routes,
		LogGuid:         desireAppMessage.LogGuid,
	}

	if desiredLRP.Instances == 0 {
		err := n.bbs.RemoveDesiredLRPByProcessGuid(desiredLRP.ProcessGuid)
		if err != nil {
			n.logger.Errord(
				map[string]interface{}{
					"desired-app-message": desireAppMessage,
					"error":               err.Error(),
				},
				"nsync.remove-desired-lrp.failed",
			)

			return
		}
	} else {
		err := n.bbs.DesireLRP(desiredLRP)
		if err != nil {
			n.logger.Errord(
				map[string]interface{}{
					"desired-app-message": desireAppMessage,
					"error":               err.Error(),
				},
				"nsync.desire-lrp.failed",
			)

			return
		}
	}
}
