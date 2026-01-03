package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

// ---------- Config ----------

type ModelConfig struct {
	Name                  string `yaml:"name" json:"name"`
	MaxCompletionTokens   int    `yaml:"max_completion_tokens" json:"max_completion_tokens"`
	ConcurrentConnections int    `yaml:"concurrent_connections" json:"concurrent_connections"`
	SupportsEmbedding     bool   `yaml:"supports_embedding" json:"supports_embedding"`
	SupportsVision        bool   `yaml:"supports_vision" json:"supports_vision"`
	Fallback              bool   `yaml:"fallback" json:"fallback"`
	Enabled               bool   `yaml:"enabled" json:"enabled"`
}

type Config struct {
	// Base URL of Andy API. Examples:
	//  http://localhost:8080  -> ws://localhost:8080/ws
	//  https://api.example.com -> wss://api.example.com/ws
	//  ws://host:port/ws (kept, /ws appended if missing)
	//  wss://host:port/ws
	AndyAPIURL string `yaml:"andy_api_url" json:"andy_api_url"`
	// Client token for user authentication (obtained from AndyAPI admin panel)
	ClientToken       string `yaml:"client_token" json:"client_token"`
	Provider          string `yaml:"provider" json:"provider"`
	HeartbeatInterval int    `yaml:"heartbeat_interval" json:"heartbeat_interval"`
	ReconnectMaxBack  int    `yaml:"reconnect_max_backoff" json:"reconnect_max_backoff"`
	// Local OpenAI-compatible provider (for scanning available models)
	LocalAPIURL  string        `yaml:"local_api_url" json:"local_api_url"`
	LocalAPIKey  string        `yaml:"local_api_key" json:"local_api_key"`
	Models       []ModelConfig `yaml:"models" json:"models"`
	LastClientID string        `yaml:"last_client_id" json:"last_client_id"`
}

func (c *Config) WSURL() string {
	base := strings.TrimSpace(c.AndyAPIURL)
	if base == "" {
		return "ws://localhost:8080/ws"
	}
	// trim trailing slashes
	for strings.HasSuffix(base, "/") {
		base = strings.TrimSuffix(base, "/")
	}
	if strings.HasPrefix(base, "ws://") || strings.HasPrefix(base, "wss://") {
		if strings.HasSuffix(base, "/ws") {
			return base
		}
		return base + "/ws"
	}
	if strings.HasPrefix(base, "http://") {
		base = "ws://" + strings.TrimPrefix(base, "http://")
	} else if strings.HasPrefix(base, "https://") {
		base = "wss://" + strings.TrimPrefix(base, "https://")
	} else {
		base = "ws://" + base
	}
	return base + "/ws"
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 30
	}
	if c.ReconnectMaxBack <= 0 {
		c.ReconnectMaxBack = 30
	}
	return c, nil
}

// ---------- WS Protocol Structures (mirrors server) ----------

type WSMessage struct {
	Type      string      `json:"type"`
	ClientID  string      `json:"client_id,omitempty"`
	RequestID string      `json:"request_id,omitempty"`
	Data      interface{} `json:"data"`
	Timestamp time.Time   `json:"timestamp"`
}

type ProvidedModel struct {
	ClientID              string  `json:"client_id"`
	Provider              string  `json:"provider"`
	Name                  string  `json:"name"`
	UpstreamID            string  `json:"upstream_id,omitempty"`
	MaxCompletionTokens   int     `json:"max_completion_tokens"`
	ConcurrentConnections int     `json:"concurrent_connections"`
	AvgTokensPerSecond    float64 `json:"avg_tokens_per_second"`
	Latency               float64 `json:"latency"`
	SuccessfulResponses   int     `json:"successful_responses"`
	FailedResponses       int     `json:"failed_responses"`
	SupportsEmbedding     bool    `json:"supports_embedding"`
	SupportsVision        bool    `json:"supports_vision"`
	Fallback              bool    `json:"fallback"`
	IsAvailable           bool    `json:"is_available"`
	IsServerModel         bool    `json:"is_server_model"`
	Tier                  string  `json:"tier,omitempty"`
}

type ClientRegistration struct {
	ID          string          `json:"id,omitempty"`
	ClientToken string          `json:"client_token,omitempty"`
	Models      []ProvidedModel `json:"models"`
}

// ChatCompletionMessage mirrors OpenAI message format
type ChatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type LocalClientRequest struct {
	ID                  int                     `json:"id"`
	Prompt              string                  `json:"prompt"`
	Messages            []ChatCompletionMessage `json:"messages,omitempty"`
	ImageBase64         string                  `json:"image_base64"`
	Model               string                  `json:"model"`
	MaxCompletionTokens int                     `json:"max_completion_tokens"`
	Task                string                  `json:"task,omitempty"` // "chat", "embedding"
}

type LocalClientResponse struct {
	ID       int    `json:"id"`
	Response string `json:"response"`
	Status   string `json:"status"`
	Error    int    `json:"error"`
}

// ---------- Runtime Client State ----------

type ProviderClient struct {
	cfg           *Config
	conn          *websocket.Conn
	mu            sync.RWMutex
	writeMu       sync.Mutex
	clientID      string
	connected     bool
	everConnected bool
	closing       chan struct{}
	closed        bool
	httpSrv       *http.Server
	configPath    string
	initialSetup  bool
	connectCtx    context.Context
	connectCancel context.CancelFunc
}

func NewProviderClient(cfg *Config, configPath string, initial bool) *ProviderClient {
	return &ProviderClient{cfg: cfg, closing: make(chan struct{}), configPath: configPath, initialSetup: initial}
}

// connect establishes WS connection with exponential backoff
func (pc *ProviderClient) connect(ctx context.Context) {
	backoff := 1
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		wsURL := pc.cfg.WSURL()

		// Append token as query parameter (backend checks both query param and header)
		if token := strings.TrimSpace(pc.cfg.ClientToken); token != "" {
			if strings.Contains(wsURL, "?") {
				wsURL = wsURL + "&token=" + token
			} else {
				wsURL = wsURL + "?token=" + token
			}
		}

		log.Printf("Connecting to Andy API WS: %s", wsURL)

		// Also send token in header as backup
		headers := http.Header{}
		if token := strings.TrimSpace(pc.cfg.ClientToken); token != "" {
			headers.Set("X-Client-Token", token)
		}

		c, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
		if err != nil {
			if resp != nil {
				log.Printf("Dial failed: %v (HTTP %d)", err, resp.StatusCode)
			} else {
				log.Printf("Dial failed: %v", err)
			}
			backoff = min(backoff*2, pc.cfg.ReconnectMaxBack)
			time.Sleep(time.Duration(backoff) * time.Second)
			continue
		}
		pc.mu.Lock()
		pc.conn = c
		pc.connected = true
		pc.mu.Unlock()
		log.Printf("WebSocket connected")
		pc.handleConnection(ctx)
		backoff = 1
		// After connection ends mark disconnected and retry on next loop
		pc.mu.Lock()
		pc.connected = false
		pc.mu.Unlock()
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handleConnection manages registration, read & heartbeat loops until disconnect
func (pc *ProviderClient) handleConnection(ctx context.Context) {
	models := make([]ProvidedModel, 0, len(pc.cfg.Models))
	lastID := strings.TrimSpace(pc.cfg.LastClientID)
	clientToken := strings.TrimSpace(pc.cfg.ClientToken)
	for _, m := range pc.cfg.Models {
		models = append(models, ProvidedModel{
			ClientID:              lastID,
			Provider:              pc.cfg.Provider,
			Name:                  m.Name,
			MaxCompletionTokens:   m.MaxCompletionTokens,
			ConcurrentConnections: m.ConcurrentConnections,
			SupportsEmbedding:     m.SupportsEmbedding,
			SupportsVision:        m.SupportsVision,
			IsAvailable:           m.Enabled,
			IsServerModel:         false,
		})
	}
	reg := WSMessage{Type: "register", ClientID: lastID, Data: ClientRegistration{ID: lastID, ClientToken: clientToken, Models: models}, Timestamp: time.Now()}
	pc.writeJSON(reg)
	// Setup pong handler to extend deadlines
	pc.conn.SetPongHandler(func(appData string) error {
		pc.conn.SetReadDeadline(time.Now().Add(time.Duration(pc.cfg.HeartbeatInterval*2) * time.Second))
		return nil
	})
	go pc.heartbeatLoop()
	pc.readLoop(ctx)
}

func (pc *ProviderClient) heartbeatLoop() {
	ticker := time.NewTicker(time.Duration(pc.cfg.HeartbeatInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-pc.closing:
			return
		case <-ticker.C:
			// Send app-level heartbeat message
			pc.writeJSON(WSMessage{Type: "heartbeat", Timestamp: time.Now()})
			// Also try a websocket-level ping to keep NATs happy
			pc.mu.RLock()
			c := pc.conn
			pc.mu.RUnlock()
			if c != nil {
				pc.writeMu.Lock()
				_ = c.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
				pc.writeMu.Unlock()
			}
		}
	}
}

func (pc *ProviderClient) readLoop(ctx context.Context) {
	for {
		pc.conn.SetReadDeadline(time.Now().Add(time.Duration(pc.cfg.HeartbeatInterval*2) * time.Second))
		_, data, err := pc.conn.ReadMessage()
		if err != nil {
			log.Printf("Read error: %v", err)
			pc.conn.Close()
			return
		}
		var msg WSMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "welcome":
			if d, ok := msg.Data.(map[string]interface{}); ok {
				if id, ok2 := d["client_id"].(string); ok2 {
					pc.mu.Lock()
					pc.clientID = id
					pc.everConnected = true
					pc.cfg.LastClientID = id
					_ = pc.saveConfig("")
					pc.mu.Unlock()
					log.Printf("Assigned client_id=%s", id)
				}
			}
		case "request":
			// Handle in background to avoid blocking the read loop (prevents ping/pong timeouts)
			go pc.handleRequest(msg)
		}
	}
}

func (pc *ProviderClient) handleRequest(msg WSMessage) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in handleRequest: %v", r)
		}
	}()
	var req LocalClientRequest
	b, _ := json.Marshal(msg.Data)
	_ = json.Unmarshal(b, &req)
	// Forward to local OpenAI-compatible API using the requested model
	respText, err := pc.callLocalCompletion(req)
	if err != nil {
		log.Printf("local completion error: %v", err)
		resp := LocalClientResponse{ID: req.ID, Response: "", Status: "error", Error: 1}
		pc.writeJSON(WSMessage{Type: "response", RequestID: msg.RequestID, ClientID: pc.clientID, Data: resp, Timestamp: time.Now()})
		return
	}
	resp := LocalClientResponse{ID: req.ID, Response: respText, Status: "ok", Error: 0}
	pc.writeJSON(WSMessage{Type: "response", RequestID: msg.RequestID, ClientID: pc.clientID, Data: resp, Timestamp: time.Now()})
}

// callLocalCompletion sends a chat completion request to the configured local OpenAI-compatible API.
func (pc *ProviderClient) callLocalCompletion(req LocalClientRequest) (string, error) {
	pc.mu.RLock()
	base := strings.TrimSpace(pc.cfg.LocalAPIURL)
	key := strings.TrimSpace(pc.cfg.LocalAPIKey)
	pc.mu.RUnlock()
	if base == "" {
		return "", fmt.Errorf("local_api_url not configured")
	}
	for strings.HasSuffix(base, "/") {
		base = strings.TrimSuffix(base, "/")
	}

	// Handle embedding task
	if req.Task == "embedding" {
		return pc.callLocalEmbedding(base, key, req)
	}

	url := base + "/v1/chat/completions"
	// Build messages: prefer Messages array, fallback to Prompt
	var messages []map[string]string
	if len(req.Messages) > 0 {
		for _, m := range req.Messages {
			messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
		}
	} else if req.Prompt != "" {
		messages = []map[string]string{{"role": "user", "content": req.Prompt}}
	} else {
		return "", fmt.Errorf("no messages or prompt provided")
	}

	payload := map[string]interface{}{
		"model":    req.Model,
		"messages": messages,
	}
	// ensure max_tokens included only if > 0
	if req.MaxCompletionTokens > 0 {
		payload["max_tokens"] = req.MaxCompletionTokens
	}
	body, _ := json.Marshal(payload)
	httpReq, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	if key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var slurp struct {
			Error interface{} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&slurp)
		return "", fmt.Errorf("local api status %s: %v", resp.Status, slurp.Error)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// callLocalEmbedding sends an embedding request to the configured local OpenAI-compatible API.
func (pc *ProviderClient) callLocalEmbedding(base, key string, req LocalClientRequest) (string, error) {
	url := base + "/v1/embeddings"
	// Build input from prompt or first message
	input := req.Prompt
	if input == "" && len(req.Messages) > 0 {
		input = req.Messages[len(req.Messages)-1].Content
	}
	payload := map[string]interface{}{
		"model": req.Model,
		"input": input,
	}
	body, _ := json.Marshal(payload)
	httpReq, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	if key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var slurp struct {
			Error interface{} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&slurp)
		return "", fmt.Errorf("local api status %s: %v", resp.Status, slurp.Error)
	}
	// Return raw JSON response for embeddings (server will parse it)
	var rawResp json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&rawResp); err != nil {
		return "", err
	}
	return string(rawResp), nil
}

func (pc *ProviderClient) writeJSON(v interface{}) {
	pc.mu.RLock()
	c := pc.conn
	pc.mu.RUnlock()
	if c == nil {
		return
	}
	// Serialize all writes; gorilla/websocket requires application-level write locking
	pc.writeMu.Lock()
	defer pc.writeMu.Unlock()
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.WriteJSON(v); err != nil {
		log.Printf("Write error: %v", err)
	}
}

// saveConfig writes current config to disk if persistence enabled
func (pc *ProviderClient) saveConfig(path string) error {
	if path == "" {
		path = pc.configPath
		if path == "" {
			path = "config.yaml"
		}
	}
	b, err := yaml.Marshal(pc.cfg)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	return os.WriteFile(path, b, 0644)
}

// ---------- Management HTTP API ----------
func (pc *ProviderClient) startHTTP(addr string) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	r.GET("/", func(c *gin.Context) { c.File("ui/index.html") })
	r.GET("/ui", func(c *gin.Context) { c.File("ui/index.html") })
	r.GET("/api/state", func(c *gin.Context) { c.JSON(200, gin.H{"initial_setup": pc.initialSetup}) })
	r.GET("/config", func(c *gin.Context) { c.JSON(200, pc.cfg) })
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok", "client_id": pc.clientID, "ws_url": pc.cfg.WSURL()})
	})
	r.GET("/api/status", func(c *gin.Context) {
		pc.mu.RLock()
		clientID := pc.clientID
		if clientID == "" {
			clientID = pc.cfg.LastClientID
		}
		status := gin.H{
			"connected":      pc.connected,
			"ever_connected": pc.everConnected || (clientID != ""),
			"client_id":      clientID,
			"ws_url":         pc.cfg.WSURL(),
		}
		pc.mu.RUnlock()
		c.JSON(200, status)
	})
	r.POST("/api/connect", func(c *gin.Context) {
		pc.StartConnect()
		c.JSON(200, gin.H{"ok": true})
	})
	r.POST("/api/disconnect", func(c *gin.Context) {
		pc.StopConnect()
		c.JSON(200, gin.H{"ok": true})
	})
	r.POST("/api/save-config", func(c *gin.Context) {
		if !pc.initialSetup {
			c.JSON(400, gin.H{"error": "not in initial setup"})
			return
		}
		var newCfg Config
		if err := c.ShouldBindJSON(&newCfg); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if strings.TrimSpace(newCfg.AndyAPIURL) == "" {
			c.JSON(400, gin.H{"error": "andy_api_url required"})
			return
		}
		if strings.TrimSpace(newCfg.Provider) == "" {
			newCfg.Provider = "provider"
		}
		if len(newCfg.Models) == 0 {
			c.JSON(400, gin.H{"error": "at least one model"})
			return
		}
		pc.mu.Lock()
		pc.cfg = &newCfg
		pc.initialSetup = false
		pc.mu.Unlock()
		if err := pc.saveConfig(""); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		pm := []ProvidedModel{}
		for _, m := range newCfg.Models {
			pm = append(pm, ProvidedModel{Provider: newCfg.Provider, Name: m.Name, MaxCompletionTokens: m.MaxCompletionTokens, ConcurrentConnections: m.ConcurrentConnections, SupportsEmbedding: m.SupportsEmbedding, SupportsVision: m.SupportsVision, IsAvailable: m.Enabled})
		}
		pc.writeJSON(WSMessage{Type: "update_models", Data: pm, Timestamp: time.Now()})
		c.JSON(200, gin.H{"saved": true})
	})
	r.POST("/models", func(c *gin.Context) {
		var models []ModelConfig
		if err := c.ShouldBindJSON(&models); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		pc.cfg.Models = models
		pm := []ProvidedModel{}
		for _, m := range models {
			pm = append(pm, ProvidedModel{Provider: pc.cfg.Provider, Name: m.Name, MaxCompletionTokens: m.MaxCompletionTokens, ConcurrentConnections: m.ConcurrentConnections, SupportsEmbedding: m.SupportsEmbedding, SupportsVision: m.SupportsVision, IsAvailable: m.Enabled})
		}
		pc.writeJSON(WSMessage{Type: "update_models", Data: pm, Timestamp: time.Now()})
		_ = pc.saveConfig("")
		c.JSON(200, gin.H{"updated": len(models)})
	})

	// Update full config even after initial setup (allows changing base URL, provider, etc.)
	r.POST("/api/update-config", func(c *gin.Context) {
		var newCfg Config
		if err := c.ShouldBindJSON(&newCfg); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if strings.TrimSpace(newCfg.AndyAPIURL) == "" {
			c.JSON(400, gin.H{"error": "andy_api_url required"})
			return
		}
		if strings.TrimSpace(newCfg.Provider) == "" {
			newCfg.Provider = "provider"
		}
		// Merge: if no models provided, keep existing
		if len(newCfg.Models) == 0 {
			newCfg.Models = pc.cfg.Models
		}
		pc.mu.Lock()
		pc.cfg.AndyAPIURL = newCfg.AndyAPIURL
		pc.cfg.ClientToken = newCfg.ClientToken
		pc.cfg.LocalAPIURL = newCfg.LocalAPIURL
		pc.cfg.LocalAPIKey = newCfg.LocalAPIKey
		pc.cfg.Provider = newCfg.Provider
		pc.cfg.HeartbeatInterval = newCfg.HeartbeatInterval
		pc.cfg.ReconnectMaxBack = newCfg.ReconnectMaxBack
		pc.cfg.Models = newCfg.Models
		pc.mu.Unlock()
		if err := pc.saveConfig(""); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		// Broadcast model updates
		pm := []ProvidedModel{}
		for _, m := range pc.cfg.Models {
			pm = append(pm, ProvidedModel{Provider: pc.cfg.Provider, Name: m.Name, MaxCompletionTokens: m.MaxCompletionTokens, ConcurrentConnections: m.ConcurrentConnections, SupportsEmbedding: m.SupportsEmbedding, SupportsVision: m.SupportsVision, IsAvailable: m.Enabled})
		}
		pc.writeJSON(WSMessage{Type: "update_models", Data: pm, Timestamp: time.Now()})
		c.JSON(200, gin.H{"saved": true})
	})

	// Scan models from a local OpenAI-compatible endpoint to avoid browser CORS issues.
	r.POST("/api/scan-models", func(c *gin.Context) {
		var body struct {
			BaseURL string `json:"base_url"`
			APIKey  string `json:"api_key"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		base := strings.TrimSpace(body.BaseURL)
		if base == "" {
			c.JSON(400, gin.H{"error": "base_url required"})
			return
		}
		// normalize URL
		for strings.HasSuffix(base, "/") {
			base = strings.TrimSuffix(base, "/")
		}
		url := base + "/v1/models"
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		if strings.TrimSpace(body.APIKey) != "" {
			req.Header.Set("Authorization", "Bearer "+body.APIKey)
		}
		req.Header.Set("Accept", "application/json")
		httpClient := &http.Client{Timeout: 10 * time.Second}
		resp, err := httpClient.Do(req)
		if err != nil {
			c.JSON(502, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.JSON(resp.StatusCode, gin.H{"error": "remote returned status " + resp.Status})
			return
		}
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		models := make([]ModelConfig, 0, len(payload.Data))
		for _, item := range payload.Data {
			name := strings.TrimSpace(item.ID)
			if name == "" {
				continue
			}
			m := ModelConfig{Name: name, MaxCompletionTokens: 4096, ConcurrentConnections: 1, Enabled: false}
			// crude heuristics
			lname := strings.ToLower(name)
			if strings.Contains(lname, "embed") {
				m.SupportsEmbedding = true
			}
			if strings.Contains(lname, "vision") || strings.Contains(lname, "vl") {
				m.SupportsVision = true
			}
			models = append(models, m)
		}
		c.JSON(200, gin.H{"models": models})
	})
	pc.httpSrv = &http.Server{Addr: addr, Handler: r}
	go func() {
		if err := pc.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("mgmt http error: %v", err)
		}
	}()
}

// Manual connect/disconnect controls
func (pc *ProviderClient) StartConnect() {
	pc.mu.Lock()
	if pc.connectCtx != nil {
		pc.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	pc.connectCtx = ctx
	pc.connectCancel = cancel
	pc.mu.Unlock()
	go pc.connect(ctx)
}

func (pc *ProviderClient) StopConnect() {
	pc.mu.Lock()
	if pc.connectCancel != nil {
		pc.connectCancel()
		pc.connectCancel = nil
		pc.connectCtx = nil
	}
	c := pc.conn
	pc.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// ---------- Entry Point ----------
func main() {
	rand.Seed(time.Now().UnixNano())
	cfgPath := flag.String("config", "config.yaml", "Path to config.yaml")
	example := flag.String("example", "config.example.yaml", "Path to example config (used if config missing)")
	httpAddr := flag.String("http", ":8090", "Management HTTP listen address")
	flag.Parse()
	var cfg *Config
	initial := false
	if _, err := os.Stat(*cfgPath); err != nil {
		// attempt example
		if ex, err2 := loadConfig(*example); err2 == nil {
			cfg = ex
			initial = true
			log.Printf("config not found; starting in initial-setup mode")
		} else {
			// create minimal default
			cfg = &Config{AndyAPIURL: "http://localhost:8080", Provider: "provider", HeartbeatInterval: 30, ReconnectMaxBack: 30}
			initial = true
			log.Printf("config & example missing; using defaults for initial setup")
		}
	} else {
		c, err := loadConfig(*cfgPath)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		cfg = c
	}
	client := NewProviderClient(cfg, *cfgPath, initial)
	_, cancel := context.WithCancel(context.Background())
	// Do not autoconnect; UI will call /api/connect
	client.startHTTP(*httpAddr)

	// Print startup message with instructions
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║             AndyAPI Local Client - Provider Mode                 ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════╣")
	fmt.Println("║  Web UI available at:                                            ║")
	fmt.Printf("║    → http://localhost%s                                        ║\n", *httpAddr)
	fmt.Println("║                                                                  ║")
	fmt.Println("║  Quick Start:                                                    ║")
	fmt.Println("║    1. Open the Web UI in your browser                            ║")
	fmt.Println("║    2. Enter your AndyAPI server URL                              ║")
	fmt.Println("║    3. Enter your Client Token (from AndyAPI admin panel)         ║")
	fmt.Println("║    4. Configure your Local OpenAI API URL (e.g. Ollama)          ║")
	fmt.Println("║    5. Scan or add models, then click 'Save'                      ║")
	fmt.Println("║    6. Click 'Connect to AndyAPI' to start providing models       ║")
	fmt.Println("║                                                                  ║")
	fmt.Println("║  Press Ctrl+C to stop the client.                                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	log.Println("Shutting down provider client...")
	close(client.closing)
	cancel()
	client.StopConnect()
	if client.httpSrv != nil {
		_ = client.httpSrv.Shutdown(context.Background())
	}
}
