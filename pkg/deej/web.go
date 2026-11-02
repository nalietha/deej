package deej

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

//go:embed web
var webAssets embed.FS

// webServer serves deej's control panel on localhost
type webServer struct {
	deej   *Deej
	logger *zap.SugaredLogger

	lock   sync.Mutex
	server *http.Server
	port   int
}

const (
	webListenHost = "127.0.0.1"

	// how often live pot values are pushed to the control panel
	webSerialPushInterval = 40 * time.Millisecond

	// how often the list of audio sessions is pushed (it also triggers cooldown-limited re-scans)
	webSessionsPushInterval = 2 * time.Second

	maxConfigBodySize = 1 << 20
)

func newWebServer(deej *Deej, logger *zap.SugaredLogger) *webServer {
	ws := &webServer{
		deej:   deej,
		logger: logger.Named("web"),
	}

	ws.setupOnConfigReload()

	return ws
}

// URL returns the control panel's address
func (ws *webServer) URL() string {
	return fmt.Sprintf("http://localhost:%d/", ws.deej.config.Snapshot().WebUI.Port)
}

func (ws *webServer) start() {
	webUI := ws.deej.config.Snapshot().WebUI
	if !webUI.Enabled {
		ws.logger.Info("Control panel disabled in config")
		return
	}

	ws.lock.Lock()
	defer ws.lock.Unlock()

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", webListenHost, webUI.Port))
	if err != nil {
		ws.logger.Warnw("Failed to start control panel", "port", webUI.Port, "error", err)
		ws.deej.notifier.Notify("Control panel unavailable",
			fmt.Sprintf("Port %d is taken - is another deej running? You can change it in config.yaml.", webUI.Port))

		return
	}

	ws.port = webUI.Port
	ws.server = &http.Server{Handler: ws.guard(ws.routes())}

	go func(server *http.Server) {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			ws.logger.Warnw("Control panel server stopped", "error", err)
		}
	}(ws.server)

	ws.logger.Infow("Control panel up", "url", ws.URL())
}

func (ws *webServer) stop() {
	ws.lock.Lock()
	defer ws.lock.Unlock()

	if ws.server == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// live event streams never finish by themselves, so don't wait on them for long
	if err := ws.server.Shutdown(ctx); err != nil {
		ws.server.Close()
	}

	ws.server = nil
	ws.port = 0
}

// restart the server if it was toggled or moved to another port
func (ws *webServer) setupOnConfigReload() {
	configReloadedChannel := ws.deej.config.SubscribeToChanges()

	go func() {
		for range configReloadedChannel {
			webUI := ws.deej.config.Snapshot().WebUI

			ws.lock.Lock()
			running, port := ws.server != nil, ws.port
			ws.lock.Unlock()

			if running == webUI.Enabled && (!running || port == webUI.Port) {
				continue
			}

			// let the response that triggered this reach the browser first
			go func() {
				<-time.After(300 * time.Millisecond)
				ws.stop()
				ws.start()
			}()
		}
	}()
}

func (ws *webServer) routes() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(webAssets, "web")
	mux.Handle("/", http.FileServer(http.FS(static)))

	mux.HandleFunc("/api/state", ws.handleState)
	mux.HandleFunc("/api/events", ws.handleEvents)
	mux.HandleFunc("/api/config", ws.handleConfig)
	mux.HandleFunc("/api/sessions/refresh", ws.handleRefreshSessions)
	mux.HandleFunc("/api/ports", ws.handlePorts)
	mux.HandleFunc("/api/reconnect", ws.handleReconnect)

	return mux
}

// guard keeps other websites (and other machines) away from the API: only localhost hosts are served,
// and changes must come as JSON from our own origin - which browsers won't send cross-site without a preflight we never answer
func (ws *webServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}

		if host != "localhost" && host != "127.0.0.1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
				return
			}

			if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (ws *webServer) statePayload(sessions []SessionInfo) map[string]interface{} {
	config := ws.deej.config.Snapshot()

	return map[string]interface{}{
		"config":   config,
		"profiles": sortedProfileNames(config.Profiles),
		"status":   ws.deej.serial.Status(),
		"sessions": sessions,
		"version":  ws.deej.version,
		"platform": runtime.GOOS,
	}
}

func (ws *webServer) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ws.statePayload(ws.deej.sessions.snapshot(true)))
}

func (ws *webServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, ws.deej.config.Snapshot())

	case http.MethodPut:
		config := &ConfigFile{}

		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfigBodySize)).Decode(config); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid config: " + err.Error()})
			return
		}

		if err := ws.deej.config.Save(config); err != nil {
			ws.logger.Warnw("Failed to save config from control panel", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, ws.deej.config.Snapshot())

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (ws *webServer) handleRefreshSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// performance: forcing is okay here, it's a button press away just like the tray option
	ws.deej.sessions.refreshSessions(true)
	writeJSON(w, http.StatusOK, ws.deej.sessions.snapshot(false))
}

func (ws *webServer) handlePorts(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("probe") == "" {
		ports := util.ListSerialPorts()
		writeJSON(w, http.StatusOK, ports)
		return
	}

	writeJSON(w, http.StatusOK, ws.deej.serial.ProbePorts())
}

func (ws *webServer) handleReconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ws.deej.serial.Reconnect()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleEvents streams live state to the control panel as server-sent events
func (ws *webServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var lastSerial, lastSessions []byte
	lastConfigVersion := ws.deej.config.Version()

	send := func(event string, payload []byte) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		flusher.Flush()
	}

	sendSessions := func(refresh bool) {
		payload, _ := json.Marshal(ws.deej.sessions.snapshot(refresh))
		if !bytes.Equal(payload, lastSessions) {
			lastSessions = payload
			send("sessions", payload)
		}
	}

	serialTicker := time.NewTicker(webSerialPushInterval)
	defer serialTicker.Stop()

	sessionsTicker := time.NewTicker(webSessionsPushInterval)
	defer sessionsTicker.Stop()

	sendSessions(false)

	for {
		select {
		case <-r.Context().Done():
			return

		case <-serialTicker.C:
			payload, _ := json.Marshal(ws.deej.serial.Status())
			if !bytes.Equal(payload, lastSerial) {
				lastSerial = payload
				send("serial", payload)
			}

			if version := ws.deej.config.Version(); version != lastConfigVersion {
				lastConfigVersion = version
				config := ws.deej.config.Snapshot()

				payload, _ := json.Marshal(map[string]interface{}{
					"config":   config,
					"profiles": sortedProfileNames(config.Profiles),
				})

				send("config", payload)
			}

		case <-sessionsTicker.C:
			sendSessions(true)
		}
	}
}

func (d *Deej) openControlPanel() {
	if !d.config.Snapshot().WebUI.Enabled {
		d.notifier.Notify("Control panel disabled", "Set web_ui.enabled to true in config.yaml to use it.")
		return
	}

	if err := util.OpenURL(d.web.URL()); err != nil {
		d.logger.Warnw("Failed to open control panel in browser", "error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}
