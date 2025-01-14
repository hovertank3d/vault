package gocbcore

import (
	"sync"
)

type configManagementComponent struct {
	useSSL        bool
	networkType   string
	noTLSSeedNode bool

	currentConfig *routeConfig
	configLock    sync.Mutex

	cfgChangeWatchers []routeConfigWatcher
	watchersLock      sync.Mutex

	srcServers []routeEndpoint

	seenConfig bool
}

type configManagerProperties struct {
	UseTLS        bool
	NoTLSSeedNode bool
	NetworkType   string
	SrcMemdAddrs  []routeEndpoint
	SrcHTTPAddrs  []routeEndpoint
}

type routeConfigWatcher interface {
	OnNewRouteConfig(cfg *routeConfig)
}

type configManager interface {
	AddConfigWatcher(watcher routeConfigWatcher)
	RemoveConfigWatcher(watcher routeConfigWatcher)
}

func newConfigManager(props configManagerProperties) *configManagementComponent {
	return &configManagementComponent{
		useSSL:        props.UseTLS,
		noTLSSeedNode: props.NoTLSSeedNode,
		networkType:   props.NetworkType,
		srcServers:    append(props.SrcMemdAddrs, props.SrcHTTPAddrs...),
		currentConfig: &routeConfig{
			revID: -1,
		},
	}
}

func (cm *configManagementComponent) UseTLS(use bool) {
	cm.configLock.Lock()
	cm.useSSL = use
	cm.configLock.Unlock()
}

func (cm *configManagementComponent) TLSEnabled() bool {
	cm.configLock.Lock()
	useSSL := cm.useSSL
	cm.configLock.Unlock()

	return useSSL
}

func (cm *configManagementComponent) OnNewConfig(cfg *cfgBucket) {
	var routeCfg *routeConfig
	cm.configLock.Lock()
	if cm.seenConfig {
		routeCfg = cfg.BuildRouteConfig(cm.useSSL, cm.networkType, false, cm.noTLSSeedNode)
	} else {
		routeCfg = cm.buildFirstRouteConfig(cfg, cm.useSSL)
		logDebugf("Using network type %s for connections", cm.networkType)
	}
	if !routeCfg.IsValid() {
		cm.configLock.Unlock()
		logDebugf("Routing data is not valid, skipping update: \n%s", routeCfg.DebugString())
		return
	}

	// There's something wrong with this route config so don't send it to the watchers.
	if !cm.canUpdateRouteConfig(routeCfg) {
		cm.configLock.Unlock()
		return
	}

	cm.currentConfig = routeCfg
	cm.seenConfig = true
	cm.configLock.Unlock()

	logDebugf("Sending out mux routing data (update)...")
	logDebugf("New Routing Data:\n%s", routeCfg.DebugString())

	// We can end up deadlocking if we iterate whilst in the lock and a watcher decides to remove itself.
	cm.watchersLock.Lock()
	watchers := make([]routeConfigWatcher, len(cm.cfgChangeWatchers))
	copy(watchers, cm.cfgChangeWatchers)
	cm.watchersLock.Unlock()

	for _, watcher := range watchers {
		watcher.OnNewRouteConfig(routeCfg)
	}
}

func (cm *configManagementComponent) Watchers() []routeConfigWatcher {
	cm.watchersLock.Lock()
	watchers := make([]routeConfigWatcher, len(cm.cfgChangeWatchers))
	copy(watchers, cm.cfgChangeWatchers)
	cm.watchersLock.Unlock()

	return watchers
}

func (cm *configManagementComponent) ResetConfig() {
	cm.configLock.Lock()
	cm.currentConfig = &routeConfig{
		revID: -1,
	}
	cm.configLock.Unlock()
}

func (cm *configManagementComponent) AddConfigWatcher(watcher routeConfigWatcher) {
	cm.watchersLock.Lock()
	cm.cfgChangeWatchers = append(cm.cfgChangeWatchers, watcher)
	cm.watchersLock.Unlock()
}

func (cm *configManagementComponent) RemoveConfigWatcher(watcher routeConfigWatcher) {
	var idx int
	var found bool
	cm.watchersLock.Lock()
	for i, w := range cm.cfgChangeWatchers {
		if w == watcher {
			idx = i
			found = true
			break
		}
	}

	if !found {
		cm.watchersLock.Unlock()
		return
	}

	if idx == len(cm.cfgChangeWatchers) {
		cm.cfgChangeWatchers = cm.cfgChangeWatchers[:idx]
	} else {
		cm.cfgChangeWatchers = append(cm.cfgChangeWatchers[:idx], cm.cfgChangeWatchers[idx+1:]...)
	}
	cm.watchersLock.Unlock()
}

// We should never be receiving concurrent updates and nothing should be accessing
// our internal route config so we shouldn't need to lock here.
func (cm *configManagementComponent) canUpdateRouteConfig(cfg *routeConfig) bool {
	oldCfg := cm.currentConfig

	// Check some basic things to ensure consistency!
	// If oldCfg name was empty and the new cfg isn't then we're moving from cluster to bucket connection.
	if oldCfg.revID > -1 && (oldCfg.name != "" && cfg.name != "") {
		if (cfg.vbMap == nil) != (oldCfg.vbMap == nil) {
			logErrorf("Received a configuration with a different number of vbuckets %s-%s.  Ignoring.", oldCfg.name, cfg.name)
			return false
		}

		if cfg.vbMap != nil && cfg.vbMap.NumVbuckets() != oldCfg.vbMap.NumVbuckets() {
			logErrorf("Received a configuration with a different number of vbuckets %s-%s.  Ignoring.", oldCfg.name, cfg.name)
			return false
		}
	}

	// Check that the new config data is newer than the current one, in the case where we've done a select bucket
	// against an existing connection then the revisions could be the same. In that case the configuration still
	// needs to be applied.
	// In the case where the rev epochs are the same then we need to compare rev IDs. If the new config epoch is lower
	// than the old one then we ignore it, if it's newer then we apply the new config.
	if cfg.bktType != oldCfg.bktType {
		logDebugf("Configuration data changed bucket type, switching.")
	} else if !cfg.IsNewerThan(oldCfg) {
		return false
	}

	return true
}

func (cm *configManagementComponent) buildFirstRouteConfig(config *cfgBucket, useSSL bool) *routeConfig {
	if cm.networkType != "" && cm.networkType != "auto" {
		return config.BuildRouteConfig(useSSL, cm.networkType, true, cm.noTLSSeedNode)
	}

	defaultRouteConfig := config.BuildRouteConfig(useSSL, "default", true, cm.noTLSSeedNode)

	var kvServerList []routeEndpoint
	var mgmtEpList []routeEndpoint
	if useSSL {
		kvServerList = defaultRouteConfig.kvServerList.SSLEndpoints
		mgmtEpList = defaultRouteConfig.mgmtEpList.SSLEndpoints
	} else {
		kvServerList = defaultRouteConfig.kvServerList.NonSSLEndpoints
		mgmtEpList = defaultRouteConfig.mgmtEpList.NonSSLEndpoints
	}

	// Iterate over all the source servers and check if any addresses match as default or external network types
	for _, srcServer := range cm.srcServers {
		// First we check if the source server is from the defaults list
		srcInDefaultConfig := false
		for _, endpoint := range kvServerList {
			if trimSchemePrefix(endpoint.Address) == srcServer.Address {
				srcInDefaultConfig = true
			}
		}
		for _, endpoint := range mgmtEpList {
			if endpoint == srcServer {
				srcInDefaultConfig = true
			}
		}
		if srcInDefaultConfig {
			cm.networkType = "default"
			return defaultRouteConfig
		}
	}

	// Next lets see if we have an external config, if so, default to that
	externalRouteCfg := config.BuildRouteConfig(useSSL, "external", true, cm.noTLSSeedNode)
	if externalRouteCfg.IsValid() {
		cm.networkType = "external"
		return externalRouteCfg
	}

	// If all else fails, default to the implicit default config
	cm.networkType = "default"
	return defaultRouteConfig
}

func (cm *configManagementComponent) NetworkType() string {
	return cm.networkType
}
