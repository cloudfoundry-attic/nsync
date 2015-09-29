package nsync

import (
	"time"

	"github.com/cloudfoundry-incubator/consuladapter"
	"github.com/cloudfoundry-incubator/locket"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
)

const NysncBulkerLockSchemaKey = "nsync_bulker_lock"

func NysncBulkerLockSchemaPath() string {
	return locket.LockSchemaPath(NysncBulkerLockSchemaKey)
}

type ServiceClient interface {
	NewNsyncBulkerLockRunner(logger lager.Logger, bulkerID string, retryInterval time.Duration) ifrit.Runner
}

type serviceClient struct {
	session *consuladapter.Session
	clock   clock.Clock
}

func NewServiceClient(session *consuladapter.Session, clock clock.Clock) ServiceClient {
	return serviceClient{session, clock}
}

func (c serviceClient) NewNsyncBulkerLockRunner(logger lager.Logger, bulkerID string, retryInterval time.Duration) ifrit.Runner {
	return locket.NewLock(c.session, NysncBulkerLockSchemaPath(), []byte(bulkerID), c.clock, retryInterval, logger)
}
