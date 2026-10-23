/*
 * This file is part of the KubeVirt Redfish project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2025 KubeVirt Redfish project and its authors.
 *
 */

package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/kubevirt/redfish-controller/pkg/logger"
)

// ResponseWriter wraps http.ResponseWriter to capture status code
type ResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

// WriteHeader captures the status code
func (rw *ResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Write implements the http.ResponseWriter interface
func (rw *ResponseWriter) Write(b []byte) (int, error) {
	return rw.ResponseWriter.Write(b)
}

// generateCorrelationID generates a unique correlation ID for request tracking
func generateCorrelationID() string {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		logger.Error("Failed to generate correlation ID: %v", err)
		// Fallback to timestamp-based ID if random generation fails
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

// extractUser extracts username from request (from auth middleware)
func extractUser(r *http.Request) string {
	// This will be populated by the auth middleware
	if user := r.Header.Get("X-Redfish-User"); user != "" {
		return user
	}
	return "unknown"
}

// LoggingMiddleware adds correlation IDs and request/response logging
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Generate correlation ID
		correlationID := generateCorrelationID()

		// Extract user information
		user := extractUser(r)

		// Create context with correlation ID
		ctx := logger.WithCorrelationID(r.Context(), correlationID)
		ctx = logger.WithUser(ctx, user)
		ctx = logger.WithOperation(ctx, r.Method)
		ctx = logger.WithResource(ctx, r.URL.Path)

		// Set context for logging
		logger.SetContext(ctx)

		// Log incoming request
		logger.LogRequest(r.Method, r.URL.Path, user, correlationID)

		// Wrap response writer to capture status code
		wrappedWriter := &ResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK, // Default status code
		}

		// Process request
		next.ServeHTTP(wrappedWriter, r.WithContext(ctx))

		// Calculate duration
		duration := time.Since(start)

		// Log response
		logger.LogResponse(r.Method, r.URL.Path, user, correlationID, wrappedWriter.statusCode, duration)
	})
}
