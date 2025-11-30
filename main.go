package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"bytes"
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

type LocalEndpoint struct {
	ID           string            `yaml:"id" json:"id"`
	URL          string            `yaml:"url" json:"url"`
	APIKey       string            `yaml:"api_key" json:"api_key"`
	ExtraHeaders map[string]string `yaml:"extra_headers" json:"extra_headers"`
	ExtraParams  map[string]interface{} `yaml:"extra_params" json:"extra_params"`
	EnableLogging bool             `yaml:"enable_logging" json:"enable_logging"`
	LogFile       string           `yaml:"log_file" json:"log_file"`
}

type ModelConfig struct {
	Name                  string            `yaml:"name" json:"name"`
	InternalID            string            `yaml:"internal_id" json:"internal_id"`
	EndpointID            string            `yaml:"endpoint_id" json:"endpoint_id"`
	SystemPrompt          string            `yaml:"system_prompt" json:"system_prompt"`
	MaxCompletionTokens   int               `yaml:"max_completion_tokens" json:"max_completion_tokens"`
	ConcurrentConnections int               `yaml:"concurrent_connections" json:"concurrent_connections"`
	SupportsEmbedding     bool              `yaml:"supports_embedding" json:"supports_embedding"`
	SupportsVision        bool              `yaml:"supports_vision" json:"supports_vision"`
	Fallback              bool              `yaml:"fallback" json:"fallback"`
	Enabled               bool              `yaml:"enabled" json:"enabled"`
	ExtraHeaders          map[string]string `yaml:"extra_headers" json:"extra_headers"`
	ExtraParams           map[string]interface{} `yaml:"extra_params" json:"extra_params"`
	EnableLogging         *bool             `yaml:"enable_logging" json:"enable_logging"`         // nil = inherit from endpoint
	LogFile               string            `yaml:"log_file" json:"log_file"`                     // empty = inherit from endpoint
}

type Config struct {
	// Base URL of Andy API. Examples:
	//  http://localhost:8080  -> ws://localhost:8080/ws
	//  https://api.example.com -> wss://api.example.com/ws
	//  ws://host:port/ws (kept, /ws appended if missing)
	//  wss://host:port/ws
	AndyAPIURL        string        `yaml:"andy_api_url" json:"andy_api_url"`
	AndyAPIKey        string        `yaml:"andy_api_key" json:"andy_api_key"`
	Provider          string        `yaml:"provider" json:"provider"`
	HeartbeatInterval int           `yaml:"heartbeat_interval" json:"heartbeat_interval"`
	ReconnectMaxBack  int           `yaml:"reconnect_max_backoff" json:"reconnect_max_backoff"`
	RequestTimeout    int           `yaml:"request_timeout" json:"request_timeout"` // seconds, default 60
	// Local OpenAI-compatible provider (for scanning available models)
	LocalAPIURL string          `yaml:"local_api_url" json:"local_api_url"`
	LocalAPIKey string          `yaml:"local_api_key" json:"local_api_key"`
	Endpoints   []LocalEndpoint `yaml:"endpoints" json:"endpoints"`
	Models      []ModelConfig   `yaml:"models" json:"models"`
	LastClientID      string        `yaml:"last_client_id" json:"last_client_id"`
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
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60
	}
	// Migration: if Endpoints empty but LocalAPIURL set, create default endpoint
	if len(c.Endpoints) == 0 && c.LocalAPIURL != "" {
		c.Endpoints = []LocalEndpoint{{
			ID:     "default",
			URL:    c.LocalAPIURL,
			APIKey: c.LocalAPIKey,
		}}
		// Update models to use default endpoint if not set
		for i := range c.Models {
			if c.Models[i].EndpointID == "" {
				c.Models[i].EndpointID = "default"
			}
		}
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
}

type ClientRegistration struct {
	ID     string          `json:"id,omitempty"`
	Models []ProvidedModel `json:"models"`
}

type LocalClientRequest struct {
	ID                  int    `json:"id"`
	Prompt              string `json:"prompt"`
	ImageBase64         string `json:"image_base64"`
	Model               string `json:"model"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
}

type LocalClientResponse struct {
	ID       int    `json:"id"`
	Response string `json:"response"`
	Status   string `json:"status"`
	Error    int    `json:"error"`
}

// ---------- Statistics ----------

type RequestStats struct {
	Timestamp    time.Time `json:"timestamp"`
	Model        string    `json:"model"`
	Duration     float64   `json:"duration"`      // seconds
	TokensIn     int       `json:"tokens_in"`     // estimated input tokens
	TokensOut    int       `json:"tokens_out"`    // estimated output tokens
	Success      bool      `json:"success"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

type ModelStats struct {
	TotalRequests       int     `json:"total_requests"`
	SuccessfulRequests  int     `json:"successful_requests"`
	FailedRequests      int     `json:"failed_requests"`
	TotalTokensIn       int     `json:"total_tokens_in"`
	TotalTokensOut      int     `json:"total_tokens_out"`
	TotalDuration       float64 `json:"total_duration"`
	AvgDuration         float64 `json:"avg_duration"`
	AvgTokensPerSecond  float64 `json:"avg_tokens_per_second"`
}

type Statistics struct {
	mu              sync.RWMutex
	RecentRequests  []RequestStats         `json:"recent_requests"`  // last 100 requests
	ModelStats      map[string]*ModelStats `json:"model_stats"`
	HourlyStats     []HourlyStat           `json:"hourly_stats"`     // last 24 hours
	StartTime       time.Time              `json:"start_time"`
	MaxRecentRequests int
}

type HourlyStat struct {
	Hour            time.Time `json:"hour"`
	TotalRequests   int       `json:"total_requests"`
	SuccessfulReqs  int       `json:"successful_requests"`
	FailedReqs      int       `json:"failed_requests"`
	TotalTokensIn   int       `json:"total_tokens_in"`
	TotalTokensOut  int       `json:"total_tokens_out"`
	AvgDuration     float64   `json:"avg_duration"`
}

func NewStatistics() *Statistics {
	return &Statistics{
		RecentRequests:    make([]RequestStats, 0, 100),
		ModelStats:        make(map[string]*ModelStats),
		HourlyStats:       make([]HourlyStat, 0, 24),
		StartTime:         time.Now(),
		MaxRecentRequests: 100,
	}
}

func (s *Statistics) RecordRequest(model string, duration float64, tokensIn, tokensOut int, success bool, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	req := RequestStats{
		Timestamp:    now,
		Model:        model,
		Duration:     duration,
		TokensIn:     tokensIn,
		TokensOut:    tokensOut,
		Success:      success,
		ErrorMessage: errMsg,
	}

	// Add to recent requests (keep last 100)
	s.RecentRequests = append(s.RecentRequests, req)
	if len(s.RecentRequests) > s.MaxRecentRequests {
		s.RecentRequests = s.RecentRequests[1:]
	}

	// Update model stats
	if _, ok := s.ModelStats[model]; !ok {
		s.ModelStats[model] = &ModelStats{}
	}
	ms := s.ModelStats[model]
	ms.TotalRequests++
	if success {
		ms.SuccessfulRequests++
	} else {
		ms.FailedRequests++
	}
	ms.TotalTokensIn += tokensIn
	ms.TotalTokensOut += tokensOut
	ms.TotalDuration += duration
	if ms.TotalRequests > 0 {
		ms.AvgDuration = ms.TotalDuration / float64(ms.TotalRequests)
		if ms.TotalDuration > 0 {
			ms.AvgTokensPerSecond = float64(ms.TotalTokensOut) / ms.TotalDuration
		}
	}

	// Update hourly stats
	hourKey := now.Truncate(time.Hour)
	var hourStat *HourlyStat
	for i := range s.HourlyStats {
		if s.HourlyStats[i].Hour.Equal(hourKey) {
			hourStat = &s.HourlyStats[i]
			break
		}
	}
	if hourStat == nil {
		// Clean old hours (keep 24)
		cutoff := now.Add(-24 * time.Hour)
		newHourly := make([]HourlyStat, 0, 24)
		for _, h := range s.HourlyStats {
			if h.Hour.After(cutoff) {
				newHourly = append(newHourly, h)
			}
		}
		newHourly = append(newHourly, HourlyStat{Hour: hourKey})
		s.HourlyStats = newHourly
		hourStat = &s.HourlyStats[len(s.HourlyStats)-1]
	}
	hourStat.TotalRequests++
	if success {
		hourStat.SuccessfulReqs++
	} else {
		hourStat.FailedReqs++
	}
	hourStat.TotalTokensIn += tokensIn
	hourStat.TotalTokensOut += tokensOut
	hourStat.AvgDuration = (hourStat.AvgDuration*float64(hourStat.TotalRequests-1) + duration) / float64(hourStat.TotalRequests)
}

func (s *Statistics) GetStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalReqs := 0
	totalSuccess := 0
	totalFailed := 0
	for _, ms := range s.ModelStats {
		totalReqs += ms.TotalRequests
		totalSuccess += ms.SuccessfulRequests
		totalFailed += ms.FailedRequests
	}

	return map[string]interface{}{
		"uptime_seconds":     time.Since(s.StartTime).Seconds(),
		"total_requests":     totalReqs,
		"successful_requests": totalSuccess,
		"failed_requests":    totalFailed,
		"recent_requests":    s.RecentRequests,
		"model_stats":        s.ModelStats,
		"hourly_stats":       s.HourlyStats,
	}
}

// ---------- Request/Response Logging ----------

type RequestLog struct {
	Timestamp      time.Time              `json:"timestamp"`
	RequestID      string                 `json:"request_id"`
	Model          string                 `json:"model"`
	InternalModel  string                 `json:"internal_model,omitempty"`
	EndpointID     string                 `json:"endpoint_id,omitempty"`
	Prompt         string                 `json:"prompt"`
	SystemPrompt   string                 `json:"system_prompt,omitempty"`
	ImageIncluded  bool                   `json:"image_included"`
	MaxTokens      int                    `json:"max_tokens,omitempty"`
	Response       string                 `json:"response"`
	TokensIn       int                    `json:"tokens_in"`
	TokensOut      int                    `json:"tokens_out"`
	DurationMs     int64                  `json:"duration_ms"`
	Success        bool                   `json:"success"`
	ErrorMessage   string                 `json:"error_message,omitempty"`
	ExtraParams    map[string]interface{} `json:"extra_params,omitempty"`
}

var logFileMu sync.Mutex

func writeRequestLog(logFile string, entry RequestLog) error {
	if logFile == "" {
		return nil
	}
	logFileMu.Lock()
	defer logFileMu.Unlock()

	// Ensure directory exists
	dir := filepath.Dir(logFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create log directory: %w", err)
		}
	}

	// Open file in append mode, create if not exists
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()

	// Write as JSONL (one JSON object per line)
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to marshal log entry: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write log entry: %w", err)
	}
	return nil
}

// ---------- Runtime Client State ----------

type ProviderClient struct {
	cfg          *Config
	conn         *websocket.Conn
	mu           sync.RWMutex
	writeMu      sync.Mutex
	clientID     string
	connected    bool
	everConnected bool
	closing      chan struct{}
	closed       bool
	httpSrv      *http.Server
	configPath   string
	stats        *Statistics
	configMtime  time.Time
	initialSetup bool
	connectCtx   context.Context
	connectCancel context.CancelFunc
}

func NewProviderClient(cfg *Config, configPath string, initial bool) *ProviderClient {
	pc := &ProviderClient{
		cfg:          cfg,
		closing:      make(chan struct{}),
		configPath:   configPath,
		initialSetup: initial,
		stats:        NewStatistics(),
	}
	// Get initial mtime for hot reload
	if info, err := os.Stat(configPath); err == nil {
		pc.configMtime = info.ModTime()
	}
	return pc
}

// watchConfig periodically checks for config file changes and reloads
func (pc *ProviderClient) watchConfig() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-pc.closing:
			return
		case <-ticker.C:
			info, err := os.Stat(pc.configPath)
			if err != nil {
				continue
			}
			pc.mu.RLock()
			mtime := pc.configMtime
			pc.mu.RUnlock()

			if info.ModTime().After(mtime) {
				log.Printf("Config file changed, reloading...")
				newCfg, err := loadConfig(pc.configPath)
				if err != nil {
					log.Printf("Failed to reload config: %v", err)
					continue
				}
				pc.mu.Lock()
				oldURL := pc.cfg.WSURL()
				pc.cfg = newCfg
				pc.configMtime = info.ModTime()
				newURL := pc.cfg.WSURL()
				pc.mu.Unlock()

				log.Printf("Config reloaded successfully")

				// If WS URL changed and we're connected, trigger reconnect
				if oldURL != newURL && pc.connected {
					log.Printf("WS URL changed, reconnecting...")
					pc.StopConnect()
					time.Sleep(100 * time.Millisecond)
					pc.StartConnect()
				}
			}
		}
	}
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
		log.Printf("Connecting to Andy API WS: %s", wsURL)
		c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			log.Printf("Dial failed: %v", err)
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
	reg := WSMessage{Type: "register", ClientID: lastID, Data: ClientRegistration{ID: lastID, Models: models}, Timestamp: time.Now()}
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
	startTime := time.Now()
	defer func(){
		if r := recover(); r != nil {
			log.Printf("panic in handleRequest: %v", r)
		}
	}()
	var req LocalClientRequest
	b, _ := json.Marshal(msg.Data)
	_ = json.Unmarshal(b, &req)
	// Forward to local OpenAI-compatible API using the requested model
	result := pc.callLocalCompletion(req)
	duration := time.Since(startTime)
	
	// Estimate input tokens (rough: ~4 chars per token)
	tokensIn := len(req.Prompt) / 4
	if tokensIn < 1 {
		tokensIn = 1
	}

	// Handle logging if enabled
	if result.EnableLogging && result.LogFile != "" {
		logEntry := RequestLog{
			Timestamp:     startTime,
			RequestID:     msg.RequestID,
			Model:         req.Model,
			InternalModel: result.InternalModel,
			EndpointID:    result.EndpointID,
			Prompt:        req.Prompt,
			SystemPrompt:  result.SystemPrompt,
			ImageIncluded: req.ImageBase64 != "",
			MaxTokens:     req.MaxCompletionTokens,
			Response:      result.Response,
			TokensIn:      tokensIn,
			TokensOut:     result.TokensOut,
			DurationMs:    duration.Milliseconds(),
			Success:       result.Error == nil,
			ExtraParams:   result.ExtraParams,
		}
		if result.Error != nil {
			logEntry.ErrorMessage = result.Error.Error()
		}
		if err := writeRequestLog(result.LogFile, logEntry); err != nil {
			log.Printf("Failed to write request log: %v", err)
		}
	}

	if result.Error != nil {
		log.Printf("local completion error: %v", result.Error)
		pc.stats.RecordRequest(req.Model, duration.Seconds(), tokensIn, 0, false, result.Error.Error())
		resp := LocalClientResponse{ID: req.ID, Response: "", Status: "error", Error: 1}
		pc.writeJSON(WSMessage{Type: "response", RequestID: msg.RequestID, ClientID: pc.clientID, Data: resp, Timestamp: time.Now()})
		return
	}
	
	pc.stats.RecordRequest(req.Model, duration.Seconds(), tokensIn, result.TokensOut, true, "")
	resp := LocalClientResponse{ID: req.ID, Response: result.Response, Status: "ok", Error: 0}
	pc.writeJSON(WSMessage{Type: "response", RequestID: msg.RequestID, ClientID: pc.clientID, Data: resp, Timestamp: time.Now()})
}

// CompletionResult holds the result of a local completion call including metadata for logging
type CompletionResult struct {
	Response      string
	TokensOut     int
	Error         error
	// Metadata for logging
	InternalModel string
	EndpointID    string
	SystemPrompt  string
	ExtraParams   map[string]interface{}
	EnableLogging bool
	LogFile       string
}

// callLocalCompletion sends a chat completion request to the configured local OpenAI-compatible API.
// Returns CompletionResult with response text, output tokens estimate, error, and logging metadata
func (pc *ProviderClient) callLocalCompletion(req LocalClientRequest) CompletionResult {
	pc.mu.RLock()
	cfg := pc.cfg
	pc.mu.RUnlock()

	result := CompletionResult{}

	var endpointURL, endpointKey, modelID string
	var endpointHeaders map[string]string
	var endpointParams map[string]interface{}
	var modelHeaders map[string]string
	var modelParams map[string]interface{}
	var endpointEnableLogging bool
	var endpointLogFile string

	// Find model config
	var modelCfg *ModelConfig
	for i := range cfg.Models {
		if cfg.Models[i].Name == req.Model {
			modelCfg = &cfg.Models[i]
			break
		}
	}

	var endpoint *LocalEndpoint
	if modelCfg != nil {
		// Determine Endpoint
		if modelCfg.EndpointID != "" {
			for i := range cfg.Endpoints {
				if cfg.Endpoints[i].ID == modelCfg.EndpointID {
					endpoint = &cfg.Endpoints[i]
					endpointURL = endpoint.URL
					endpointKey = endpoint.APIKey
					endpointHeaders = endpoint.ExtraHeaders
					endpointParams = endpoint.ExtraParams
					endpointEnableLogging = endpoint.EnableLogging
					endpointLogFile = endpoint.LogFile
					result.EndpointID = endpoint.ID
					break
				}
			}
		}
		// Determine Model ID (Internal vs External)
		if modelCfg.InternalID != "" {
			modelID = modelCfg.InternalID
		} else {
			modelID = modelCfg.Name
		}
		modelHeaders = modelCfg.ExtraHeaders
		modelParams = modelCfg.ExtraParams
		result.SystemPrompt = modelCfg.SystemPrompt
	} else {
		modelID = req.Model
	}

	result.InternalModel = modelID

	// Fallback to legacy/default if no endpoint found
	if endpointURL == "" {
		endpointURL = cfg.LocalAPIURL
		endpointKey = cfg.LocalAPIKey
	}

	if endpointURL == "" && len(cfg.Endpoints) > 0 {
		// Fallback to first endpoint if available
		endpoint = &cfg.Endpoints[0]
		endpointURL = endpoint.URL
		endpointKey = endpoint.APIKey
		endpointHeaders = endpoint.ExtraHeaders
		endpointParams = endpoint.ExtraParams
		endpointEnableLogging = endpoint.EnableLogging
		endpointLogFile = endpoint.LogFile
		result.EndpointID = endpoint.ID
	}

	// Determine logging settings (model overrides endpoint)
	if modelCfg != nil && modelCfg.EnableLogging != nil {
		result.EnableLogging = *modelCfg.EnableLogging
	} else {
		result.EnableLogging = endpointEnableLogging
	}
	if modelCfg != nil && modelCfg.LogFile != "" {
		result.LogFile = modelCfg.LogFile
	} else if endpointLogFile != "" {
		result.LogFile = endpointLogFile
	} else if result.EnableLogging {
		// Default log file if logging enabled but no file specified
		result.LogFile = "logs/requests.jsonl"
	}

	base := strings.TrimSpace(endpointURL)
	key := strings.TrimSpace(endpointKey)

	if base == "" {
		result.Error = fmt.Errorf("no endpoint configured for model %s", req.Model)
		return result
	}
	for strings.HasSuffix(base, "/") {
		base = strings.TrimSuffix(base, "/")
	}
	url := base + "/chat/completions"

	messages := []map[string]string{}
	if modelCfg != nil && modelCfg.SystemPrompt != "" {
		messages = append(messages, map[string]string{"role": "system", "content": modelCfg.SystemPrompt})
	}
	messages = append(messages, map[string]string{"role": "user", "content": req.Prompt})

	// Minimal OpenAI format
	payload := map[string]interface{}{
		"model":    modelID,
		"messages": messages,
	}
	// ensure max_tokens included only if > 0
	if req.MaxCompletionTokens > 0 {
		payload["max_completion_tokens"] = req.MaxCompletionTokens
	}

	// Apply extra params from endpoint (base layer)
	for k, v := range endpointParams {
		payload[k] = v
	}
	// Apply extra params from model (override layer - model takes priority)
	for k, v := range modelParams {
		payload[k] = v
	}

	// Collect merged extra params for logging (excluding messages and model)
	mergedParams := make(map[string]interface{})
	for k, v := range endpointParams {
		mergedParams[k] = v
	}
	for k, v := range modelParams {
		mergedParams[k] = v
	}
	if len(mergedParams) > 0 {
		result.ExtraParams = mergedParams
	}

	body, _ := json.Marshal(payload)
	httpReq, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	if key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}

	// Apply extra headers from endpoint (base layer)
	for k, v := range endpointHeaders {
		httpReq.Header.Set(k, v)
	}
	// Apply extra headers from model (override layer - model takes priority)
	for k, v := range modelHeaders {
		httpReq.Header.Set(k, v)
	}

	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 60
	}
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		result.Error = err
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var slurp struct {
			Error interface{} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&slurp)
		result.Error = fmt.Errorf("local api status %s: %v", resp.Status, slurp.Error)
		return result
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		result.Error = err
		return result
	}
	if len(out.Choices) == 0 {
		result.Error = fmt.Errorf("no choices")
		return result
	}
	
	content := out.Choices[0].Message.Content
	tokensOut := out.Usage.CompletionTokens
	// Estimate if not provided
	if tokensOut == 0 {
		tokensOut = len(content) / 4
		if tokensOut < 1 {
			tokensOut = 1
		}
	}
	result.Response = content
	result.TokensOut = tokensOut
	return result
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
	r.GET("/api/stats", func(c *gin.Context) {
		c.JSON(200, pc.stats.GetStats())
	})
	r.POST("/api/stats/reset", func(c *gin.Context) {
		pc.stats = NewStatistics()
		c.JSON(200, gin.H{"ok": true})
	})
	r.GET("/api/status", func(c *gin.Context) {
		pc.mu.RLock()
		clientID := pc.clientID
		if clientID == "" {
			clientID = pc.cfg.LastClientID
		}
		status := gin.H{
			"connected": pc.connected,
			"ever_connected": pc.everConnected || (clientID != ""),
			"client_id": clientID,
			"ws_url": pc.cfg.WSURL(),
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
		pc.cfg.AndyAPIKey = newCfg.AndyAPIKey
		pc.cfg.LocalAPIURL = newCfg.LocalAPIURL
		pc.cfg.LocalAPIKey = newCfg.LocalAPIKey
		pc.cfg.Endpoints = newCfg.Endpoints
		pc.cfg.Provider = newCfg.Provider
		pc.cfg.HeartbeatInterval = newCfg.HeartbeatInterval
		pc.cfg.ReconnectMaxBack = newCfg.ReconnectMaxBack
		pc.cfg.RequestTimeout = newCfg.RequestTimeout
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
		var body struct{
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
		for strings.HasSuffix(base, "/") { base = strings.TrimSuffix(base, "/") }
		url := base + "/models"
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		if strings.TrimSpace(body.APIKey) != "" {
			req.Header.Set("Authorization", "Bearer "+body.APIKey)
		}
		req.Header.Set("Accept", "application/json")
		httpClient := &http.Client{ Timeout: 10 * time.Second }
		resp, err := httpClient.Do(req)
		if err != nil {
			c.JSON(502, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.JSON(resp.StatusCode, gin.H{"error": "remote returned status "+resp.Status})
			return
		}
		var payload struct{
			Data []struct{ ID string `json:"id"` } `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		models := make([]ModelConfig, 0, len(payload.Data))
		for _, item := range payload.Data {
			name := strings.TrimSpace(item.ID)
			if name == "" { continue }
			m := ModelConfig{ Name: name, MaxCompletionTokens: 4096, ConcurrentConnections: 1, Enabled: false }
			// crude heuristics
			lname := strings.ToLower(name)
			if strings.Contains(lname, "embed") { m.SupportsEmbedding = true }
			if strings.Contains(lname, "vision") || strings.Contains(lname, "vl") { m.SupportsVision = true }
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
			cfg = &Config{AndyAPIURL: "http://localhost:8080", Provider: "provider", HeartbeatInterval: 30, ReconnectMaxBack: 30, RequestTimeout: 60}
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
	
	// Start config hot reload watcher
	go client.watchConfig()
	log.Printf("Management UI available at http://localhost%s/ui", *httpAddr)


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
