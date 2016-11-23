package config

import (
	"encoding/json"
	"io/ioutil"
	"time"

	"code.cloudfoundry.org/debugserver"
	"code.cloudfoundry.org/lager/lagerflags"
	"code.cloudfoundry.org/locket"
)

type BulkerConfig struct {
	BBSAddress                 string                        `json:"bbs_api_url"`
	BBSCACert                  string                        `json:"bbs_ca_cert"`
	BBSCancelTaskPoolSize      int                           `json:"bbs_cancel_task_pool_size"`
	BBSClientCert              string                        `json:"bbs_client_cert"`
	BBSClientConnectionPerHost int                           `json:"bbs_client_connection_per_host"`
	BBSClientKey               string                        `json:"bbs_client_key"`
	BBSClientSessionCacheSize  int                           `json:"bbs_client_cache_size"`
	BBSFailTaskPoolSize        int                           `json:"bbs_fail_task_pool_size"`
	BBSMaxIdleConnsPerHost     int                           `json:"bbs_max_idle_conns_per_host"`
	BBSUpdateLRPWorkers        int                           `json:"bbs_update_lrp_workers"`
	CCBaseUrl                  string                        `json:"cc_base_url"`
	CCBulkBatchSize            uint                          `json:"cc_bulk_batch_size"`
	CCPassword                 string                        `json:"cc_basic_auth_password"`
	CCPollingInterval          time.Duration                 `json:"cc_polling_interval_in_seconds"`
	CCUsername                 string                        `json:"cc_basic_auth_username"`
	CommunicationTimeout       time.Duration                 `json:"communication_timeout_in_seconds"`
	ConsulCluster              string                        `json:"consul_cluster"`
	DebugServerConfig          debugserver.DebugServerConfig `json:"debug_server_config"`
	DomainTTL                  time.Duration                 `json:"domain_ttl"`
	DropsondePort              int                           `json:"dropsonde_port"`
	FileServerUrl              string                        `json:"file_server_url"`
	LagerConfig                lagerflags.LagerConfig        `json:"lager_config"`
	LockRetryInterval          time.Duration                 `json:"lock_retry_interval"`
	LockTTL                    time.Duration                 `json:"lock_ttl"`
	Lifecycles                 []string                      `json:"lifecycle_bundles"`
	PrivilegedContainers       bool                          `json:"diego_privileged_containers"`
	SkipCertVerify             bool                          `json:"skip_cert_verify"`
}

type ListenerConfig struct {
	BBSAddress                string                        `json:"bbs_api_url"`
	BBSCACert                 string                        `json:"bbs_ca_cert"`
	BBSClientCert             string                        `json:"bbs_client_cert"`
	BBSClientKey              string                        `json:"bbs_client_key"`
	BBSClientSessionCacheSize int                           `json:"bbs_client_cache_size"`
	BBSMaxIdleConnsPerHost    int                           `json:"bbs_max_idle_conns_per_host"`
	CommunicationTimeout      time.Duration                 `json:"communication_timeout_in_seconds"`
	ConsulCluster             string                        `json:"consul_cluster"`
	DebugServerConfig         debugserver.DebugServerConfig `json:"debug_server_config"`
	DropsondePort             int                           `json:"dropsonde_port"`
	FileServerURL             string                        `json:"file_server_url"`
	Lifecycles                []string                      `json:"lifecycle_bundles"`
	ListenAddress             string                        `json:"nsync_listen_addr"`
	LagerConfig               lagerflags.LagerConfig        `json:"lager_config"`
	PrivilegedContainers      bool                          `json:"diego_privileged_containers"`
}

func DefaultBulkerConfig() BulkerConfig {
	return BulkerConfig{
		BBSCancelTaskPoolSize:     50,
		BBSClientSessionCacheSize: 0,
		BBSFailTaskPoolSize:       50,
		BBSMaxIdleConnsPerHost:    0,
		BBSUpdateLRPWorkers:       50,
		CCBulkBatchSize:           500,
		CCPollingInterval:         30,
		CommunicationTimeout:      30,
		DomainTTL:                 2,
		DropsondePort:             3457,
		LagerConfig:               lagerflags.DefaultLagerConfig(),
		LockRetryInterval:         locket.RetryInterval,
		LockTTL:                   locket.LockTTL,
		PrivilegedContainers:      false,
		SkipCertVerify:            false,
	}
}

func NewBulkerConfig(configPath string) (BulkerConfig, error) {
	configFile, err := ioutil.ReadFile(configPath)
	if err != nil {
		return BulkerConfig{}, err
	}

	bulkerConfig := DefaultBulkerConfig()

	err = json.Unmarshal(configFile, &bulkerConfig)
	if err != nil {
		return BulkerConfig{}, err
	}

	bulkerConfig.CCPollingInterval = bulkerConfig.CCPollingInterval * time.Second
	bulkerConfig.CommunicationTimeout = bulkerConfig.CommunicationTimeout * time.Second
	bulkerConfig.DomainTTL = bulkerConfig.DomainTTL * time.Minute

	return bulkerConfig, nil
}

func DefaultListenerConfig() ListenerConfig {
	return ListenerConfig{
		BBSClientSessionCacheSize: 0,
		BBSMaxIdleConnsPerHost:    0,
		CommunicationTimeout:      30,
		DropsondePort:             3457,
		LagerConfig:               lagerflags.DefaultLagerConfig(),
		PrivilegedContainers:      false,
	}
}
func NewListenerConfig(configPath string) (ListenerConfig, error) {
	configFile, err := ioutil.ReadFile(configPath)
	if err != nil {
		return ListenerConfig{}, err
	}

	listenerConfig := DefaultListenerConfig()
	err = json.Unmarshal(configFile, &listenerConfig)
	if err != nil {
		return ListenerConfig{}, err
	}

	listenerConfig.CommunicationTimeout = listenerConfig.CommunicationTimeout * time.Second

	return listenerConfig, nil
}
