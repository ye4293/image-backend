package config

import "os"

// DevDefaultJWTSecret 是 dev 模式的默认 JWT 密钥，非 dev 模式启动时会拒绝使用该值。
const DevDefaultJWTSecret = "dev-secret-change-me"

type Config struct {
	Port        string
	DatabaseURL string
	JWTSecret   string
}

func Load() *Config {
	return &Config{
		Port:        getEnv("PORT", "8080"),
		DatabaseURL: getEnv("DATABASE_URL", ""),
		JWTSecret:   getEnv("JWT_SECRET", DevDefaultJWTSecret),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
