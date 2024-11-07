package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/time/rate"

	"lilmail/internal/auth"
)

// CustomResponseWriter wraps http.ResponseWriter to capture status code and size
type CustomResponseWriter struct {
	http.ResponseWriter
	status int
	size   int64
}

func (w *CustomResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *CustomResponseWriter) Write(b []byte) (int, error) {
	size, err := w.ResponseWriter.Write(b)
	w.size += int64(size)
	return size, err
}

// SecurityHeaders adds security headers to all responses
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline';")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")

		next.ServeHTTP(w, r)
	})
}

// RateLimiter implements a token bucket rate limiter per IP
type RateLimiter struct {
	visitors map[string]*rate.Limiter
	mu       sync.RWMutex
	rate     rate.Limit
	burst    int
}

func NewRateLimiter(r rate.Limit, b int) *RateLimiter {
	return &RateLimiter{
		visitors: make(map[string]*rate.Limiter),
		rate:     r,
		burst:    b,
	}
}

func (rl *RateLimiter) getLimiter(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.visitors[ip]
	if !exists {
		limiter = rate.NewLimiter(rl.rate, rl.burst)
		rl.visitors[ip] = limiter
	}

	return limiter
}

func (rl *RateLimiter) RateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
			ip = strings.Split(forwardedFor, ",")[0]
		}

		limiter := rl.getLimiter(ip)
		if !limiter.Allow() {
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Logger provides detailed logging of requests and responses
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := middleware.GetReqID(r.Context())

		// Create custom response writer to capture status and size
		cw := &CustomResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		// Process request
		next.ServeHTTP(cw, r)

		// Log request details
		duration := time.Since(start)
		log := map[string]interface{}{
			"timestamp":   time.Now().Format(time.RFC3339),
			"request_id":  requestID,
			"remote_ip":   r.RemoteAddr,
			"method":      r.Method,
			"path":        r.URL.Path,
			"status":      cw.status,
			"duration_ms": duration.Milliseconds(),
			"size_bytes":  cw.size,
			"user_agent":  r.UserAgent(),
			"referer":     r.Referer(),
		}

		if username := r.Context().Value("username"); username != nil {
			log["username"] = username
		}

		jsonLog, _ := json.Marshal(log)
		fmt.Println(string(jsonLog))
	})
}

// Recoverer recovers from panics and logs the error
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rvr := recover(); rvr != nil {
				log := map[string]interface{}{
					"timestamp":  time.Now().Format(time.RFC3339),
					"request_id": middleware.GetReqID(r.Context()),
					"error":      fmt.Sprint(rvr),
					"stacktrace": string(debug.Stack()),
				}
				jsonLog, _ := json.Marshal(log)
				fmt.Println(string(jsonLog))

				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// CacheControl adds cache control headers based on the request
func CacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't cache API responses
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		} else {
			// Cache static assets
			w.Header().Set("Cache-Control", "public, max-age=31536000")
		}

		next.ServeHTTP(w, r)
	})
}

// RequestValidator validates incoming requests
type RequestValidator struct {
	maxBodySize    int64
	allowedMethods []string
}

func NewRequestValidator(maxBodySize int64, allowedMethods []string) *RequestValidator {
	return &RequestValidator{
		maxBodySize:    maxBodySize,
		allowedMethods: allowedMethods,
	}
}

func (rv *RequestValidator) Validate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check method
		methodAllowed := false
		for _, method := range rv.allowedMethods {
			if r.Method == method {
				methodAllowed = true
				break
			}
		}
		if !methodAllowed {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Check content length
		if r.ContentLength > rv.maxBodySize {
			http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
			return
		}

		// Validate content type for POST/PUT/PATCH
		if r.Method != "GET" && r.Method != "DELETE" {
			contentType := r.Header.Get("Content-Type")
			if !strings.Contains(contentType, "application/json") {
				http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// SessionContext adds user session information to the context
func SessionContext(authManager *auth.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie("session")
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			session, err := authManager.ValidateSession(cookie.Value)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			ctx := context.WithValue(r.Context(), "session", session)
			ctx = context.WithValue(ctx, "username", session.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Metrics tracks request metrics
type Metrics struct {
	totalRequests    int64
	activeRequests   int64
	requestDurations []time.Duration
	statusCodes      map[int]int64
	mu               sync.RWMutex
}

func NewMetrics() *Metrics {
	return &Metrics{
		statusCodes: make(map[int]int64),
	}
}

func (m *Metrics) Track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.totalRequests++
		m.activeRequests++
		m.mu.Unlock()

		start := time.Now()

		cw := &CustomResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)

		duration := time.Since(start)

		m.mu.Lock()
		m.activeRequests--
		m.requestDurations = append(m.requestDurations, duration)
		m.statusCodes[cw.status]++
		m.mu.Unlock()
	})
}

// GetMetrics returns current metrics
func (m *Metrics) GetMetrics() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var totalDuration time.Duration
	for _, d := range m.requestDurations {
		totalDuration += d
	}

	var avgDuration time.Duration
	if len(m.requestDurations) > 0 {
		avgDuration = totalDuration / time.Duration(len(m.requestDurations))
	}

	return map[string]interface{}{
		"total_requests":  m.totalRequests,
		"active_requests": m.activeRequests,
		"avg_duration_ms": avgDuration.Milliseconds(),
		"status_codes":    m.statusCodes,
	}
}
