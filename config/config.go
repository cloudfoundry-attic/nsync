package config

import (
	"encoding/json"
	"io/ioutil"
	"time"
)

type BulkerConfig struct {
	BBSAddress                 string        `json:"bbs_api_url"`
	BBSCACert                  string        `json:"bbs_ca_cert"`
	BBSCancelTaskPoolSize      int           `json:"bbs_cancel_task_pool_size"`
	BBSClientCert              string        `json:"bbs_client_cert"`
	BBSClientConnectionPerHost int           `json:"bbs_client_connection_per_host"`
	BBSClientKey               string        `json:"bbs_client_key"`
	BBSClientSessionCacheSize  int           `json:"bbs_client_cache_size"`
	BBSFailTaskPoolSize        int           `json:"bbs_fail_task_pool_size"`
	BBSMaxIdleConnsPerHost     int           `json:"bbs_max_idle_conns_per_host"`
	BBSUpdateLRPWorkers        int           `json:"bbs_update_lrp_workers"`
	CCBaseUrl                  string        `json:"cc_base_url"`
	CCBulkBatchSize            uint          `json:"cc_bulk_batch_size"`
	CCPassword                 string        `json:"cc_basic_auth_password"`
	CCPollingInterval          time.Duration `json:"cc_polling_interval_in_seconds"`
	CCTimeout                  time.Duration `json:"cc_fetch_timeout_in_seconds"`
	CCUsername                 string        `json:"cc_basic_auth_username"`
	ConsulCluster              string        `json:"consul_cluster"`
	DomainTTL                  time.Duration `json:"domain_ttl"`
	DropsondePort              int           `json:"dropsonde_port"`
	FileServerUrl              string        `json:"file_server_url"`
	PrivilegedContainers       bool          `json:"diego_privileged_containers"`
	SkipCertVerify             bool          `json:"skip_cert_verify"`
}

func New(configPath string) (BulkerConfig, error) {
	configFile, err := ioutil.ReadFile(configPath)
	if err != nil {
		return BulkerConfig{}, err
	}

	bulkerConfig := BulkerConfig{
		BBSCancelTaskPoolSize:     50,
		BBSClientSessionCacheSize: 0,
		BBSFailTaskPoolSize:       50,
		BBSMaxIdleConnsPerHost:    0,
		BBSUpdateLRPWorkers:       50,
		CCBulkBatchSize:           500,
		CCPollingInterval:         30,
		CCTimeout:                 30,
		DomainTTL:                 2,
		DropsondePort:             3457,
		PrivilegedContainers:      false,
		SkipCertVerify:            false,
	}
	err = json.Unmarshal(configFile, &bulkerConfig)
	if err != nil {
		return BulkerConfig{}, err
	}

	bulkerConfig.CCPollingInterval = bulkerConfig.CCPollingInterval * time.Second
	bulkerConfig.CCTimeout = bulkerConfig.CCTimeout * time.Second
	bulkerConfig.DomainTTL = bulkerConfig.DomainTTL * time.Minute

	return bulkerConfig, nil
}
