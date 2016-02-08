package volmaster

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/contiv/volplugin/config"
	"github.com/contiv/volplugin/storage"
	"github.com/contiv/volplugin/storage/backend/ceph"
	"github.com/coreos/etcd/client"
	"github.com/gorilla/mux"
)

type daemonConfig struct {
	config   *config.TopLevelConfig
	mountTTL int
	timeout  time.Duration
}

// volume is the json response of a volume. Taken from
// https://github.com/docker/docker/blob/master/volume/drivers/adapter.go#L75
type volume struct {
	Name       string
	Mountpoint string
}

type volumeList struct {
	Volumes []volume
	Err     string
}

// volumeGet is taken from this struct in docker:
// https://github.com/docker/docker/blob/master/volume/drivers/proxy.go#L180
type volumeGet struct {
	Volume volume
	Err    string
}

// Daemon initializes the daemon for use.
func Daemon(config *config.TopLevelConfig, ttl, timeout int, debug bool, listen string) {
	d := daemonConfig{config: config,
		mountTTL: ttl,
		timeout:  time.Duration(timeout) * time.Minute}

	r := mux.NewRouter()

	postRouter := map[string]func(http.ResponseWriter, *http.Request){
		"/request":      d.handleRequest,
		"/create":       d.handleCreate,
		"/mount":        d.handleMount,
		"/mount-report": d.handleMountReport,
		"/unmount":      d.handleUnmount,
		"/remove":       d.handleRemove,
	}

	for path, f := range postRouter {
		r.HandleFunc(path, logHandler(path, debug, f)).Methods("POST")
	}

	getRouter := map[string]func(http.ResponseWriter, *http.Request){
		"/list":                  d.handleList,
		"/get/{tenant}/{volume}": d.handleGet,
	}

	for path, f := range getRouter {
		r.HandleFunc(path, logHandler(path, debug, f)).Methods("GET")
	}

	if debug {
		r.HandleFunc("{action:.*}", d.handleDebug)
	}

	if err := http.ListenAndServe(listen, r); err != nil {
		log.Fatalf("Error starting volmaster: %v", err)
	}

	select {}
}

func logHandler(name string, debug bool, actionFunc func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if debug {
			buf := new(bytes.Buffer)
			io.Copy(buf, r.Body)
			log.Debugf("Dispatching %s with %v", name, strings.TrimSpace(string(buf.Bytes())))
			var writer *io.PipeWriter
			r.Body, writer = io.Pipe()
			go func() {
				io.Copy(writer, buf)
				writer.Close()
			}()
		}

		actionFunc(w, r)
	}
}

func (d daemonConfig) handleDebug(w http.ResponseWriter, r *http.Request) {
	io.Copy(os.Stderr, r.Body)
	w.WriteHeader(404)
}

func (d daemonConfig) handleList(w http.ResponseWriter, r *http.Request) {
	vols, err := d.config.ListAllVolumes()
	if err != nil {
		httpError(w, "Retrieving list", err)
		return
	}

	response := volumeList{Volumes: []volume{}}

	for _, vol := range vols {
		parts := strings.SplitN(vol, "/", 2)
		// FIXME make this take a single string and not a split one
		volConfig, err := d.config.GetVolume(parts[0], parts[1])
		if err != nil {
			httpError(w, "Retrieving list", err)
			return
		}

		response.Volumes = append(response.Volumes, volume{Name: vol, Mountpoint: ceph.MountPath(volConfig.Options.Pool, vol)})
	}

	content, err := json.Marshal(response)
	if err != nil {
		httpError(w, "Retrieving list", err)
		return
	}

	w.Write(content)
}

func (d daemonConfig) handleGet(w http.ResponseWriter, r *http.Request) {
	volName := strings.TrimPrefix(r.URL.Path, "/get/")
	parts := strings.SplitN(volName, "/", 2)

	volConfig, err := d.config.GetVolume(parts[0], parts[1])
	if err != nil {
		log.Warn(err)
		w.WriteHeader(404)
		return
	}

	vol := volume{Name: volName, Mountpoint: ceph.MountPath(volConfig.Options.Pool, volName)}

	content, err := json.Marshal(volumeGet{Volume: vol})
	if err != nil {
		httpError(w, "Retrieving volume", err)
		return
	}

	w.Write(content)
}

func (d daemonConfig) handleRemove(w http.ResponseWriter, r *http.Request) {
	req, err := unmarshalRequest(r)
	if err != nil {
		httpError(w, "unmarshalling request", err)
		return
	}

	vc, err := d.config.GetVolume(req.Tenant, req.Volume)
	if err != nil {
		httpError(w, "obtaining volume configuration", err)
		return
	}

	hostname, err := os.Hostname()
	if err != nil {
		httpError(w, "Retrieving hostname", err)
		return
	}

	uc := &config.UseConfig{
		Volume:   vc,
		Reason:   "Remove",
		Hostname: hostname,
	}

	if err := d.config.PublishUse(uc); err != nil {
		httpError(w, "Creating use lock", err)
		return
	}

	defer d.config.RemoveUse(uc, false)

	if err := removeVolume(vc, d.timeout); err != nil {
		httpError(w, "removing image", err)
		return
	}

	if err := d.config.RemoveVolume(req.Tenant, req.Volume); err != nil {
		httpError(w, "clearing volume records", err)
		return
	}
}

func (d daemonConfig) handleUnmount(w http.ResponseWriter, r *http.Request) {
	req, err := unmarshalUseConfig(r)
	if err != nil {
		httpError(w, "Unmarshalling request", err)
		return
	}

	mt, err := d.config.GetUse(req.Volume)
	if err != nil {
		httpError(w, "Could not retrieve mount information", err)
		return
	}

	if mt.Hostname == req.Hostname {
		req.Reason = "Mount"
		if err := d.config.RemoveUse(req, false); err != nil {
			httpError(w, "Could not publish mount information", err)
			return
		}
	}
}

func (d daemonConfig) handleMountWithTTLFlag(w http.ResponseWriter, r *http.Request, exist client.PrevExistType) error {
	req, err := unmarshalUseConfig(r)
	if err != nil {
		return err
	}

	req.Reason = "Mount"

	if err := d.config.PublishUseWithTTL(req, time.Duration(d.mountTTL)*time.Second, exist); err != nil {
		return err
	}

	return nil
}

func (d daemonConfig) handleMount(w http.ResponseWriter, r *http.Request) {
	if err := d.handleMountWithTTLFlag(w, r, client.PrevNoExist); err != nil {
		httpError(w, "Could not publish mount information", err)
		return
	}
}

func (d daemonConfig) handleMountReport(w http.ResponseWriter, r *http.Request) {
	if err := d.handleMountWithTTLFlag(w, r, client.PrevExist); err != nil {
		httpError(w, "Could not publish mount information", err)
		return
	}
}

func (d daemonConfig) handleRequest(w http.ResponseWriter, r *http.Request) {
	req, err := unmarshalRequest(r)
	if err != nil {
		httpError(w, "Unmarshalling request", err)
		return
	}

	tenConfig, err := d.config.GetVolume(req.Tenant, req.Volume)
	if err == nil {
		content, err := json.Marshal(tenConfig)
		if err != nil {
			httpError(w, "Marshalling response", err)
			return
		}

		w.Write(content)
		return
	}

	w.WriteHeader(404)
}

func (d daemonConfig) handleCreate(w http.ResponseWriter, r *http.Request) {
	content, err := ioutil.ReadAll(r.Body)
	if err != nil {
		httpError(w, "Reading request", err)
		return
	}

	var req config.RequestCreate

	if err := json.Unmarshal(content, &req); err != nil {
		httpError(w, "Unmarshalling request", err)
		return
	}

	if req.Tenant == "" {
		httpError(w, "Reading tenant", errors.New("tenant was blank"))
		return
	}

	if req.Volume == "" {
		httpError(w, "Reading tenant", errors.New("volume was blank"))
		return
	}

	volConfig, err := d.config.CreateVolume(req)
	if err != nil {
		httpError(w, "Creating volume", err)
		return
	}

	tenant, err := d.config.GetTenant(req.Tenant)
	if err != nil {
		httpError(w, "Retrieving tenant", err)
		return
	}

	hostname, err := os.Hostname()
	if err != nil {
		httpError(w, "Creating volume", err)
		return
	}

	uc := &config.UseConfig{
		Volume:   volConfig,
		Reason:   "Create",
		Hostname: hostname,
	}

	if err := d.config.PublishUse(uc); err == nil {
		defer func() {
			if err := d.config.RemoveUse(uc, false); err != nil {
				log.Errorf("Could not remove use lock on create for %q", hostname)
			}
		}()

		do, err := createVolume(tenant, volConfig, d.timeout)
		if err == storage.ErrVolumeExist {
			log.Errorf("Volume exists, cleaning up")
			goto finish
		} else if err != nil {
			httpError(w, "Creating volume", err)
			return
		}

		if err := d.config.PublishVolume(volConfig); err != nil && err != config.ErrExist {
			httpError(w, "Publishing volume", err)
			return
		}

		if err := formatVolume(volConfig, do); err != nil {
			httpError(w, "Formatting volume", err)
			return
		}
	}

finish:

	content, err = json.Marshal(volConfig)
	if err != nil {
		httpError(w, "Marshalling response", err)
		return
	}

	w.Write(content)
}
