package http

import (
	"encoding/json"
	"net/http"
	"office/internal/service"
	"office/internal/transport/usb"
)

// ReaderHandler preserves the USB request ID so network retries are idempotent.
func ReaderHandler(handle usb.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()
		r.Body = http.MaxBytesReader(w, r.Body, 2048)
		var req service.ReaderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		response := handle(r.Context(), req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.Status)
		_ = json.NewEncoder(w).Encode(response)
	})
}
