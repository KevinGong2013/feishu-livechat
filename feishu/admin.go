package feishu

import (
	"encoding/json"
	"net/http"
	"strings"
)

// AdminHandler serves the operator page's API. Mount it behind the host's
// own admin authorization — anyone who can call it can connect a different
// app. basePath is stripped like livechat.HandlerOptions.BasePath.
//
//	GET    /status                                    Status; poll it while a setup is in progress
//	POST   /setup      {"app_name","group_name"}      start the scan flow → SetupStatus with the link
//	GET    /setup/qr.png                              the link as a QR code to scan with 飞书
//	DELETE /setup                                     abandon the flow in progress
//	POST   /join-link  {"validity"}                   new join link for agents (week, year, permanently) → {"url"}
func (r *Runtime) AdminHandler(basePath string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, r.Status())
	})
	mux.HandleFunc("POST /setup", func(w http.ResponseWriter, req *http.Request) {
		var o struct {
			AppName   string `json:"app_name"`
			GroupName string `json:"group_name"`
		}
		if req.ContentLength != 0 {
			req.Body = http.MaxBytesReader(w, req.Body, 4<<10)
			if err := json.NewDecoder(req.Body).Decode(&o); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
				return
			}
		}
		st, err := r.StartSetup(req.Context(), SetupOptions{AppName: strings.TrimSpace(o.AppName), GroupName: strings.TrimSpace(o.GroupName)})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("GET /setup/qr.png", func(w http.ResponseWriter, _ *http.Request) {
		png, ok := r.SetupQRCode()
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no setup waiting for a scan"})
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(png)
	})
	mux.HandleFunc("DELETE /setup", func(w http.ResponseWriter, _ *http.Request) {
		r.CancelSetup()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /join-link", func(w http.ResponseWriter, req *http.Request) {
		var o struct {
			Validity string `json:"validity"`
		}
		req.Body = http.MaxBytesReader(w, req.Body, 4<<10)
		_ = json.NewDecoder(req.Body).Decode(&o)
		switch o.Validity {
		case "":
			o.Validity = "week"
		case "week", "year", "permanently":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "validity must be week, year or permanently"})
			return
		}
		link, err := r.ShareLink(req.Context(), o.Validity)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": link})
	})
	basePath = strings.TrimSuffix(basePath, "/")
	if basePath == "" {
		return mux
	}
	return http.StripPrefix(basePath, mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
