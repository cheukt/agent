package networking

import (
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"net"
	"net/http"
	"os"
	"time"

	errw "github.com/pkg/errors"
)

type templateData struct {
	Manufacturer string
	Model        string
	FragmentID   string

	Banner       string
	LastNetwork  NetworkInfo
	VisibleSSIDs []NetworkInfo
	Errors       []string
	IsConfigured bool
	IsOnline     bool
}

//go:embed templates/*
var templates embed.FS

func (n *Networking) startPortal(inputChan chan<- userInput) error {
	n.dataMu.Lock()
	defer n.dataMu.Unlock()
	n.portalData = &portalData{input: &userInput{}, inputChan: inputChan}

	if err := n.startGRPC(); err != nil {
		return errw.Wrap(err, "starting GRPC service")
	}

	if err := n.startWeb(); err != nil {
		return errw.Wrap(err, "starting web portal service")
	}

	return nil
}

func (n *Networking) startWeb() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", n.portalIndex)
	mux.HandleFunc("/save", n.portalSave)
	n.webServer = &http.Server{
		Handler:     mux,
		ReadTimeout: time.Second * 10,
	}
	bind := PortalBindAddr + ":80"
	lis, err := net.Listen("tcp", bind)
	if err != nil {
		return errw.Wrapf(err, "listening on: %s", bind)
	}

	n.portalData.workers.Add(1)
	go func() {
		defer n.portalData.workers.Done()
		err := n.webServer.Serve(lis)
		if !errors.Is(err, http.ErrServerClosed) {
			n.logger.Error(err)
		}
	}()
	return nil
}

func (n *Networking) stopPortal() error {
	if n.grpcServer != nil {
		n.grpcServer.Stop()
		n.grpcServer = nil
	}

	var err error
	if n.webServer != nil {
		err = n.webServer.Close()
	}

	n.portalData.mu.Lock()
	defer n.portalData.mu.Unlock()
	if n.portalData.cancel != nil {
		n.portalData.cancel()
	}
	n.portalData.workers.Wait()
	n.portalData = &portalData{input: &userInput{}}

	return err
}

func (n *Networking) portalIndex(resp http.ResponseWriter, req *http.Request) {
	defer func() {
		if err := req.Body.Close(); err != nil {
			n.logger.Error(err)
		}
	}()
	n.connState.setLastInteraction()

	cfg := n.Config()

	data := templateData{
		Manufacturer: cfg.Manufacturer,
		Model:        cfg.Model,
		FragmentID:   cfg.FragmentID,
		Banner:       n.banner.Get(),
		LastNetwork:  n.getLastNetworkTried(),
		VisibleSSIDs: n.getVisibleNetworks(),
		IsOnline:     n.connState.getOnline(),
		IsConfigured: n.connState.getConfigured(),
		Errors:       n.errListAsStrings(),
	}

	t, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		n.logger.Error(err)
		http.Error(resp, err.Error(), http.StatusInternalServerError)
	}

	if os.Getenv("VIAM_AGENT_DEVMODE") != "" {
		n.logger.Warn("devmode enabled, using templates from /opt/viam/tmp/templates/")
		newT, err := template.ParseGlob("/opt/viam/tmp/templates/*.html")
		if err == nil {
			t = newT
		}
	}

	err = t.Execute(resp, data)
	if err != nil {
		n.logger.Error(err)
		http.Error(resp, err.Error(), http.StatusInternalServerError)
	}

	// reset the errors and banner, as they were now just displayed
	n.banner.Set("")
	n.errors.Clear()
}

func (n *Networking) portalSave(resp http.ResponseWriter, req *http.Request) {
	defer func() {
		if err := req.Body.Close(); err != nil {
			n.logger.Error(err)
		}
	}()
	defer http.Redirect(resp, req, "/", http.StatusSeeOther)

	if req.Method != http.MethodPost {
		return
	}

	n.connState.setLastInteraction()

	ssid := req.FormValue("ssid")
	psk := req.FormValue("password")
	rawConfig := req.FormValue("viamconfig")

	if ssid == "" && !n.connState.getOnline() {
		n.errors.Add(errors.New("no SSID provided"))
		return
	}

	if rawConfig == "" && !n.connState.getConfigured() {
		n.errors.Add(errors.New("no device config provided"))
		return
	}

	n.portalData.mu.Lock()
	defer n.portalData.mu.Unlock()
	if rawConfig != "" {
		// we'll check if the config is valid, but NOT use the parsed config, in case additional fields are in the json
		cfg := &MachineConfig{}
		if err := json.Unmarshal([]byte(rawConfig), cfg); err != nil {
			n.errors.Add(errw.Wrap(err, "invalid json config contents"))
			return
		}
		if cfg.Cloud.ID == "" || cfg.Cloud.Secret == "" || cfg.Cloud.AppAddress == "" {
			n.errors.Add(errors.New("incomplete cloud config provided"))
			return
		}
		n.portalData.input.RawConfig = rawConfig
		n.logger.Debug("saving raw device config")
		n.banner.Set("Saving device config. ")
	}

	if ssid != "" {
		n.portalData.input.SSID = ssid
		n.portalData.input.PSK = psk
		n.logger.Debugf("saving credentials for %s", n.portalData.input.SSID)
		n.banner.Set(n.banner.Get() + "Added credentials for SSID: " + n.portalData.input.SSID)
	}

	if ssid == n.netState.LastSSID(n.Config().HotspotInterface) && ssid != "" {
		lastNetwork := n.netState.LockingNetwork(n.Config().HotspotInterface, ssid)
		lastNetwork.mu.Lock()
		lastNetwork.lastError = nil
		lastNetwork.mu.Unlock()
	}
	n.portalData.Updated = time.Now()
	n.portalData.sendInput(n.connState)
}
