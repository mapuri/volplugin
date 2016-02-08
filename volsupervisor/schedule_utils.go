package volsupervisor

import (
	"github.com/contiv/volplugin/config"

	log "github.com/Sirupsen/logrus"
)

type volumeDispatch struct {
	daemonConfig *DaemonConfig
	tenant       string
	volumes      map[string]*config.VolumeConfig
}

func iterateVolumes(dc *DaemonConfig, dispatch func(v *volumeDispatch)) {
	tenants, err := dc.Config.ListTenants()
	if err != nil {
		log.Warnf("Could not locate any tenant information; sleeping from error: %v.", err)
		return
	}

	for _, tenant := range tenants {
		volumes, err := dc.Config.ListVolumes(tenant)
		if err != nil {
			log.Warnf("Could not list volumes for tenant %q: sleeping.", tenant)
			return
		}

		dispatch(&volumeDispatch{dc, tenant, volumes})
	}
}
