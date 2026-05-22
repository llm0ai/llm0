package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Server
	Port        string
	Environment string

	// Database
	DatabaseURL string

	// Redis - Optimized for high performance
	RedisURL      string
	RedisPassword string
	RedisDB       int

	// Redis Pool Optimization (from rate_limiter_go)
	RedisPoolSize        int
	RedisMinIdleConns    int
	RedisMaxRetries      int
	RedisMinRetryBackoff time.Duration
	RedisMaxRetryBackoff time.Duration
	RedisDialTimeout     time.Duration
	RedisReadTimeout     time.Duration
	RedisWriteTimeout    time.Duration
	RedisPoolTimeout     time.Duration

	// Performance
	MaxConcurrentRequests int
	RequestTimeout        time.Duration

	// TLS Configuration
	TLSEnabled          bool
	TLSCertFile         string
	TLSKeyFile          string
	TLSSessionCacheSize int

	// Gateway-specific
	OpenAIAPIKey    string
	AnthropicAPIKey string
	GeminiAPIKey    string

	// Ollama (local models)
	OllamaBaseURL string // e.g. http://localhost:11434/v1 — leave empty to disable

	// Failover routing strategy
	// Accepted values: cloud_first (default) | local_first | local_only | cloud_only
	FailoverMode string

	// Tier-to-local-model mapping used when Ollama is in the chain.
	// "flagship" = gpt-4o class, "balanced" = gpt-4o-mini class, "budget" = gpt-3.5 class.
	OllamaModelFlagship string // e.g. llama3.3:70b
	OllamaModelBalanced string // e.g. qwen2.5:14b
	OllamaModelBudget   string // e.g. gemma3:4b

	// Drop Ollama SSE chunks with empty content/tool_calls (role-only noise).
	// Set OLLAMA_FILTER_EMPTY_CHUNKS=false for raw byte-for-byte streams.
	OllamaFilterEmptyChunks bool

	// Cache
	CacheTTLSeconds int
	HotKeyCacheTTL  int // Longer TTL for frequently used keys

	// In-memory cache for customer_limits table. Every request carrying a
	// customer ID reads this table; caching avoids a Postgres round trip per
	// request. Stale reads are bounded by this TTL.
	CustomerLimitCacheTTLSeconds int

	// Embedding Service (Phase 2: Semantic Caching)
	EmbeddingServiceURL string // URL to embedding service (e.g., http://llm0-embedding-service.internal:8080)

	// Background workers (monthly spend reset, log / cache cleanup,
	// customer-spend reconciliation). Set to true in multi-replica
	// deployments where only one replica should run maintenance jobs, or in
	// tests where you don't want scheduled goroutines running. Defaults to
	// false — workers run alongside the HTTP server in the same process.
	DisableBackgroundWorkers bool
}

func Load() *Config {
	return &Config{
		// Server
		Port:        getEnv("PORT", "8080"),
		Environment: getEnv("ENVIRONMENT", "local"),

		// Database
		DatabaseURL: getEnv("DATABASE_URL", "postgres://llm0_user:llm0_password@localhost:5432/llm0_gateway?sslmode=disable"),

		// Redis
		RedisURL:      getEnv("REDIS_URL", "redis://localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       getEnvAsInt("REDIS_DB", 0),

		// Redis Pool Optimization (from rate_limiter_go)
		RedisPoolSize:        getEnvAsInt("REDIS_POOL_SIZE", 200),     // High concurrency
		RedisMinIdleConns:    getEnvAsInt("REDIS_MIN_IDLE_CONNS", 50), // Keep connections warm
		RedisMaxRetries:      getEnvAsInt("REDIS_MAX_RETRIES", 1),     // Fail fast
		RedisMinRetryBackoff: getEnvAsDuration("REDIS_MIN_RETRY_BACKOFF", 1*time.Millisecond),
		RedisMaxRetryBackoff: getEnvAsDuration("REDIS_MAX_RETRY_BACKOFF", 5*time.Millisecond),
		RedisDialTimeout:     getEnvAsDuration("REDIS_DIAL_TIMEOUT", 500*time.Millisecond),
		RedisReadTimeout:     getEnvAsDuration("REDIS_READ_TIMEOUT", 100*time.Millisecond),
		RedisWriteTimeout:    getEnvAsDuration("REDIS_WRITE_TIMEOUT", 100*time.Millisecond),
		RedisPoolTimeout:     getEnvAsDuration("REDIS_POOL_TIMEOUT", 500*time.Millisecond),

		// Performance
		MaxConcurrentRequests: getEnvAsInt("MAX_CONCURRENT_REQUESTS", 10000),
		RequestTimeout:        getEnvAsDuration("REQUEST_TIMEOUT", 30*time.Second),

		// TLS
		TLSEnabled:          getEnvAsBool("TLS_ENABLED", false),
		TLSCertFile:         getEnv("TLS_CERT_FILE", ""),
		TLSKeyFile:          getEnv("TLS_KEY_FILE", ""),
		TLSSessionCacheSize: getEnvAsInt("TLS_SESSION_CACHE_SIZE", 1000),

		// Gateway
		OpenAIAPIKey:    getEnv("OPENAI_API_KEY", ""),
		AnthropicAPIKey: getEnv("ANTHROPIC_API_KEY", ""),
		GeminiAPIKey:    getEnv("GEMINI_API_KEY", ""),

		// Ollama
		OllamaBaseURL: getEnv("OLLAMA_BASE_URL", ""),

		// Failover routing
		FailoverMode:        getEnv("FAILOVER_MODE", "cloud_first"),
		OllamaModelFlagship: getEnv("OLLAMA_MODEL_FLAGSHIP", "llama3.3:70b"),
		OllamaModelBalanced: getEnv("OLLAMA_MODEL_BALANCED", "qwen2.5:14b"),
		OllamaModelBudget:   getEnv("OLLAMA_MODEL_BUDGET", "gemma3:4b"),
		OllamaFilterEmptyChunks: getEnvAsBool("OLLAMA_FILTER_EMPTY_CHUNKS", true),

		// Cache
		CacheTTLSeconds: getEnvAsInt("CACHE_TTL_SECONDS", 3600),  // 1 hour default
		HotKeyCacheTTL:  getEnvAsInt("HOT_KEY_CACHE_TTL", 86400), // 24 hours for hot keys

		// Customer limit cache (hot-path DB read avoidance)
		CustomerLimitCacheTTLSeconds: getEnvAsInt("CUSTOMER_LIMIT_CACHE_TTL_SECONDS", 60),

		// Embedding Service (Phase 2)
		EmbeddingServiceURL: getEnv("EMBEDDING_SERVICE_URL", ""), // Empty = semantic cache disabled

		// Background workers
		DisableBackgroundWorkers: getEnvAsBool("DISABLE_BACKGROUND_WORKERS", false),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getEnvAsBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return defaultValue
}

func getEnvAsDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}
