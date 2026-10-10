package api

import (
	"encoding/json"
	"github.com/Resinat/Resin/internal/service"
	"net/http"
)

func registerGovernance(mux *http.ServeMux, cp *service.ControlPlaneService) {
	mux.HandleFunc("GET /api/v1/governance", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]any{"protocol": 1, "configured": cp.Router.GovernanceReady()})
	})
	mux.HandleFunc("POST /api/v1/governance/route", func(w http.ResponseWriter, r *http.Request) {
		if !cp.Router.GovernanceConfigured() {
			WriteJSON(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		var in struct {
			PlatformName string `json:"platformName"`
			Identity     string `json:"identity"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || in.PlatformName == "" || in.Identity == "" {
			w.WriteHeader(400)
			return
		}
		if !cp.Router.PlatformGoverned(in.PlatformName) {
			WriteJSON(w, 409, map[string]string{"status": "unmanaged"})
			return
		}
		result, err := cp.Router.RouteRequest(in.PlatformName, in.Identity, "")
		if err != nil {
			WriteJSON(w, 503, map[string]string{"status": "waiting"})
			return
		}
		WriteJSON(w, 200, map[string]string{"status": "allowed", "ip": result.EgressIP.String(), "nodeHash": result.NodeHash.Hex()})
	})
}
