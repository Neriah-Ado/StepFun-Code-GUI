// Command step-orchestra-gateway is the Go half of the step-orchestra plugin.
//
// It reads newline-delimited JSON from stdin (fed by the Step Code extension),
// reconstructs the orchestration tree, and serves a liquid-glass browser panel
// over loopback HTTP with a server-sent event stream.
//
// Security posture: the listener is bound to 127.0.0.1 only, every data
// endpoint requires a per-run bearer token, and the working directory is never
// exposed beyond what the extension explicitly forwards.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultPort  = 47810
	portAttempts = 12
	// Generous ceiling for a single JSONL line after extension-side redaction.
	maxLineBytes = 4 << 20
	heartbeat    = 15 * time.Second
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("[step-orchestra] ")

	hub := NewHub()
	store := NewStore(hub, os.Getenv("STEP_ORCHESTRA_HOME"))

	go readStdin(store)

	token := randomToken()
	webDir := resolveWebDir()

	listener, port, err := listen()
	if err != nil {
		log.Fatalf("cannot bind loopback listener: %v", err)
	}

	// The panel URL is scraped from stdout by the extension and surfaced through
	// the /orchestra command, so the token never lands in a log file.
	fmt.Printf("[step-orchestra] panel http://127.0.0.1:%d/?t=%s\n", port, token)
	fmt.Printf("[step-orchestra] assets %s\n", webDir)
	_ = os.Stdout.Sync()

	if err := http.Serve(listener, newRouter(store, hub, newActionWriter(os.Stdout), token, webDir)); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

// BridgeActionProfile carries one credential to the extension. This is the only
// message that ever contains a plaintext key, and it never leaves the host.
type BridgeActionProfile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	APIKey   string `json:"apiKey"`
	BaseURL  string `json:"baseUrl,omitempty"`
}

// BridgeAction mirrors the extension-side contract for the reverse channel.
type BridgeAction struct {
	Type      string               `json:"type"`
	Action    string               `json:"action"`
	RequestID string               `json:"requestId"`
	Text      string               `json:"text,omitempty"`
	DeliverAs string               `json:"deliverAs,omitempty"`
	Profile   *BridgeActionProfile `json:"profile,omitempty"`
}

// actionWriter serialises reverse-channel actions onto gateway stdout, where
// the extension picks them up line by line. This is the only path from the
// browser back into the host — stdin carries everything else.
type actionWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func newActionWriter(out io.Writer) *actionWriter {
	return &actionWriter{out: out}
}

func (w *actionWriter) send(action BridgeAction) error {
	payload, err := json.Marshal(action)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	_, err = fmt.Fprintf(w.out, "%s\n", payload)
	return err
}

// readStdin consumes the extension's JSONL stream until the pipe closes.
func readStdin(store *Store) {
	// One malformed frame must not take down the whole gateway: the panel can
	// survive a dropped update, but not a dead process.
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("stdin reader panicked: %v", recovered)
		}
	}()

	// A hand-rolled reader instead of bufio.Scanner: once Scanner hits a line
	// longer than its buffer it is permanently dead, and the only recovery is
	// exiting. Here an oversized line is drained chunk by chunk and skipped.
	reader := bufio.NewReaderSize(os.Stdin, 256*1024)
	var line []byte

	handle := func(raw []byte) {
		text := strings.TrimSpace(string(raw))
		if text == "" {
			return
		}

		var message incoming
		if err := json.Unmarshal([]byte(text), &message); err != nil {
			log.Printf("dropping malformed line: %v", err)
			return
		}

		if message.Kind == "shutdown" {
			log.Printf("extension host shut down; exiting")
			os.Exit(0)
		}

		store.Apply(message)
	}

	for {
		chunk, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			line = append(line, chunk...)
			if len(line) > maxLineBytes {
				discarded := len(line)
				line = line[:0]
				for err == bufio.ErrBufferFull {
					chunk, err = reader.ReadSlice('\n')
					discarded += len(chunk)
				}
				log.Printf("dropping oversized line (%d bytes)", discarded)
			}
			if err != nil {
				break
			}
			continue
		}

		line = append(line, chunk...)
		// handle copies out of the buffer before returning, so it is safe to reuse.
		handle(line)
		line = line[:0]

		if err != nil {
			if err != io.EOF {
				log.Printf("stdin read error: %v", err)
			}
			break
		}
	}

	log.Printf("stdin closed; exiting")
	os.Exit(0)
}

// listen binds the first available loopback port at or after the configured base.
func listen() (net.Listener, int, error) {
	base := defaultPort
	if raw := os.Getenv("STEP_ORCHESTRA_PORT"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed < 65536 {
			base = parsed
		}
	}

	var lastErr error
	for offset := 0; offset < portAttempts; offset++ {
		port := base + offset
		if port > 65535 {
			break
		}

		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return listener, port, nil
		}
		lastErr = err
	}

	return nil, 0, lastErr
}

func newRouter(store *Store, hub *Hub, actions *actionWriter, token, webDir string) http.Handler {
	mux := http.NewServeMux()

	// Reverse channel: the browser asks, the extension sends.
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var body struct {
			Text      string `json:"text"`
			DeliverAs string `json:"deliverAs"`
		}
		// 64 KiB is far above any realistic prompt; anything larger is abuse.
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}

		text := strings.TrimSpace(body.Text)
		if text == "" {
			http.Error(w, "text is required", http.StatusBadRequest)
			return
		}

		requestID := newRequestID()
		if err := actions.send(BridgeAction{
			Type:      "action",
			Action:    "send",
			RequestID: requestID,
			Text:      text,
			DeliverAs: body.DeliverAs,
		}); err != nil {
			http.Error(w, "reverse channel unavailable", http.StatusServiceUnavailable)
			return
		}

		writeJSON(w, map[string]any{"requestId": requestID, "accepted": true})
	})

	// Credential roster. GET returns masked views only; the plaintext key is
	// written here but never read back out over HTTP.
	mux.HandleFunc("/api/profiles", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		profiles := store.Profiles()

		switch r.Method {
		case http.MethodGet:
			views, activeID := profiles.Views()
			writeJSON(w, map[string]any{
				"profiles":  views,
				"activeId":  activeID,
				"lastError": profiles.LastError(),
			})

		case http.MethodPost:
			var body struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Provider string `json:"provider"`
				APIKey   string `json:"apiKey"`
				BaseURL  string `json:"baseUrl"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
				http.Error(w, "invalid json body", http.StatusBadRequest)
				return
			}

			saved, err := profiles.Upsert(storedProfile{
				ID:       strings.TrimSpace(body.ID),
				Name:     strings.TrimSpace(body.Name),
				Provider: strings.TrimSpace(body.Provider),
				APIKey:   strings.TrimSpace(body.APIKey),
				BaseURL:  strings.TrimSpace(body.BaseURL),
			})
			if err != nil {
				status := http.StatusInternalServerError
				if errors.Is(err, errProfileInvalid) {
					status = http.StatusBadRequest
				}
				http.Error(w, err.Error(), status)
				return
			}

			store.BroadcastProfiles()
			writeJSON(w, map[string]any{"id": saved.ID})

		case http.MethodDelete:
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				http.Error(w, "id is required", http.StatusBadRequest)
				return
			}
			if err := profiles.Remove(id); err != nil {
				http.Error(w, "could not persist profile", http.StatusInternalServerError)
				return
			}

			store.BroadcastProfiles()
			writeJSON(w, map[string]any{"removed": id})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Activation: persist the choice, then push the credential to the host.
	mux.HandleFunc("/api/profiles/activate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}

		profile, found := store.Profiles().Find(strings.TrimSpace(body.ID))
		if !found {
			http.Error(w, "unknown profile", http.StatusNotFound)
			return
		}
		if strings.TrimSpace(profile.APIKey) == "" {
			http.Error(w, "profile has no api key", http.StatusBadRequest)
			return
		}

		// Deliver first, persist second. The other order would leave the file
		// claiming a switch the host never applied whenever the channel is down.
		//
		// registerProvider applied after the load phase takes effect on the next
		// request, so the host needs neither a reload nor a restart.
		requestID := newRequestID()
		if err := actions.send(BridgeAction{
			Type:      "action",
			Action:    "apply_profile",
			RequestID: requestID,
			Profile: &BridgeActionProfile{
				ID:       profile.ID,
				Name:     profile.Name,
				Provider: profile.Provider,
				APIKey:   profile.APIKey,
				BaseURL:  profile.BaseURL,
			},
		}); err != nil {
			http.Error(w, "reverse channel unavailable", http.StatusServiceUnavailable)
			return
		}

		if _, _, err := store.Profiles().Activate(profile.ID); err != nil {
			http.Error(w, "credential delivered but activation not persisted", http.StatusInternalServerError)
			return
		}

		store.BroadcastProfiles()
		writeJSON(w, map[string]any{"activated": profile.ID, "requestId": requestID})
	})

	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		serveEvents(w, r, store, hub)
	})

	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(store.Snapshot())
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, "ok clients=%d\n", hub.Count())
	})

	mux.Handle("/", staticHandler(webDir))

	return mux
}

// serveEvents streams state deltas. A full snapshot is sent first so a
// reconnecting tab renders immediately without a separate fetch.
func serveEvents(w http.ResponseWriter, r *http.Request, store *Store, hub *Hub) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Subscribe BEFORE building the snapshot: frames broadcast in between land
	// in the channel buffer and are replayed after the initial frame. Any that
	// the snapshot already covers are dropped client-side as stale (their seq
	// is <= the snapshot's), so the ordering is safe in both directions.
	channel := hub.Subscribe()
	defer hub.Unsubscribe(channel)

	if payload, err := json.Marshal(store.SnapshotFrame()); err == nil {
		if _, err := w.Write(sseFrame(payload)); err != nil {
			return
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case frame, open := <-channel:
			if !open {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func staticHandler(webDir string) http.Handler {
	if webDir == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "web assets not found; set STEP_ORCHESTRA_WEB", http.StatusNotFound)
		})
	}

	files := http.FileServer(http.Dir(webDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
}

// writeJSON emits a no-store JSON response.
func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(payload)
}

// authorized accepts the token as a query parameter (initial page load) or a
// header (programmatic clients), always compared in constant time.
func authorized(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	if provided := r.URL.Query().Get("t"); provided != "" && constantTimeEqual(provided, token) {
		return true
	}
	if header := r.Header.Get("X-Orchestra-Token"); header != "" && constantTimeEqual(header, token) {
		return true
	}
	return false
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}

	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// resolveWebDir finds the bundled assets, preferring the explicit override.
func resolveWebDir() string {
	var candidates []string

	if fromEnv := os.Getenv("STEP_ORCHESTRA_WEB"); fromEnv != "" {
		candidates = append(candidates, fromEnv)
	}
	if executable, err := os.Executable(); err == nil {
		base := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(base, "web"),
			filepath.Join(base, "..", "web"),
		)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "..", "web"))
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

// newRequestID identifies one reverse-channel round trip.
func newRequestID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer)
}

func randomToken() string {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer)
}
