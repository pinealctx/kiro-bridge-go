package api

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"kiro-bridge-go/config"
	"kiro-bridge-go/cw"
	"kiro-bridge-go/token"
)

// Server holds the HTTP server and its dependencies.
type Server struct {
	cfg    *config.Config
	tm     *token.Manager
	client *cw.Client
	engine *gin.Engine
}

// NewServer creates and configures the HTTP server.
func NewServer(cfg *config.Config, tm *token.Manager, client *cw.Client) *Server {
	if cfg.LogLevel != "debug" && !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	s := &Server{cfg: cfg, tm: tm, client: client, engine: r}

	// Middleware
	r.Use(corsMiddleware())
	r.Use(requestIDMiddleware())
	r.Use(loggerMiddleware())

	// Routes
	r.GET("/", s.handleRoot)
	r.GET("/health", s.handleHealth)
	r.GET("/metrics", s.handleMetrics)

	// OpenAI routes
	r.GET("/v1/models", s.checkAuth, s.handleListModels)
	r.POST("/v1/chat/completions", s.checkAuth, s.handleChatCompletions)

	// Anthropic routes
	r.POST("/v1/messages", s.checkAuth, s.handleMessages)
	r.POST("/v1/messages/count_tokens", s.checkAuth, s.handleCountTokens)
	r.POST("/v1/messages/batches", s.checkAuth, s.handleBatchesNotSupported)
	r.GET("/v1/messages/batches", s.checkAuth, s.handleBatchesNotSupported)

	return s
}

// Run starts the HTTP server.
func (s *Server) Run() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	log.Printf("Starting kiro-bridge-go on %s", addr)
	return s.engine.Run(addr)
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := c.GetHeader("x-request-id")
		if reqID == "" {
			reqID = uuid.New().String()[:8]
		}
		c.Header("x-request-id", reqID)
		c.Set("request_id", reqID)
		c.Next()
	}
}

func loggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		status := c.Writer.Status()
		log.Printf("%s %s %s%d%s %v",
			c.Request.Method, c.Request.URL.Path,
			statusColor(status), status, "\033[0m",
			time.Since(start))
	}
}

func statusColor(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "\033[32m" // green
	case code >= 400 && code < 500:
		return "\033[33m" // yellow
	case code >= 500:
		return "\033[31m" // red
	default:
		return "" // no color
	}
}

func (s *Server) checkAuth(c *gin.Context) {
	if s.cfg.APIKey == "" {
		c.Next()
		return
	}
	key := ""
	auth := c.GetHeader("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		key = auth[7:]
	} else if k := c.GetHeader("x-api-key"); k != "" {
		key = k
	}
	if !timingSafeEqual(key, s.cfg.APIKey) {
		c.AbortWithStatusJSON(401, gin.H{"error": gin.H{"message": "Invalid API key", "type": "authentication_error"}})
		return
	}
	c.Next()
}

// timingSafeEqual compares two strings in constant time.
func timingSafeEqual(a, b string) bool {
	if len(a) != len(b) {
		// Still do a comparison to avoid timing leak on length
		_ = a == b
		return false
	}
	result := byte(0)
	for i := 0; i < len(a); i++ {
		result |= a[i] ^ b[i]
	}
	return result == 0
}

func (s *Server) handleRoot(c *gin.Context) {
	c.JSON(200, gin.H{
		"service": "kiro-bridge-go",
		"version": "1.0.0",
		"status":  "ok",
	})
}

func (s *Server) handleHealth(c *gin.Context) {
	token, err := s.tm.GetAccessToken(s.cfg.IdcRefreshURL)
	if err != nil {
		c.JSON(503, gin.H{"status": "unhealthy", "error": err.Error()})
		return
	}
	_ = token
	c.JSON(200, gin.H{
		"status":   "healthy",
		"endpoint": s.cfg.CodeWhispererURL,
	})
}

func (s *Server) handleMetrics(c *gin.Context) {
	c.JSON(200, gin.H{"status": "metrics disabled"})
}

func (s *Server) handleBatchesNotSupported(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"type":    "not_supported_error",
		"message": "Message batches are not supported by this gateway",
	})
}
