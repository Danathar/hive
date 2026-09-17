package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// handleConvergenceConfigGet returns the convergence rollout block plus the
// resolved effective mode and its captured generation.
func (s *Server) handleConvergenceConfigGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, s.convergenceSectionResponse(s.deps.Config))
}

// handleConvergenceConfigPut updates convergence.mode. An invalid mode is
// rejected with 400 BEFORE any live mutation or persistence — the previous
// effective mode/generation remains active, and an unknown value can never
// silently select enforcement.
func (s *Server) handleConvergenceConfigPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	mode, ok := config.NormalizeConvergenceMode(body.Mode)
	if !ok {
		jsonError(w, fmt.Sprintf("invalid convergence mode %q: must be one of %s",
			body.Mode, strings.Join(config.ConvergenceModes(), ", ")), http.StatusBadRequest)
		return
	}

	// --- apply ---
	cfg := s.deps.Config
	previous := cfg.ConvergenceMode()
	cfg.Convergence.Mode = mode
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after convergence mode update", "error", err)
	}
	s.auditFromRequest(r, "config_convergence",
		auditDetail("section", "convergence", "mode", mode, "previous_effective", previous), "")
	if s.logger != nil && previous != cfg.ConvergenceMode() {
		s.logger.Info("convergence rollout mode updated via runtime settings",
			"mode", cfg.ConvergenceMode(), "previous", previous)
	}
	jsonResponse(w, s.convergenceSectionResponse(cfg))
}

// convergenceSectionResponse renders the convergence block for the dashboard:
// the CONFIGURED mode (what the file says), the EFFECTIVE mode (after the
// HIVE_CONVERGENCE_MODE override), whether that override is in force, and the
// captured generation the eval loop is judging under.
func (s *Server) convergenceSectionResponse(cfg *config.Config) map[string]interface{} {
	// Resolve through the shared helper so an unset mode reports the real
	// default (#7260) instead of showing "off" selected in the UI while
	// shadow is what is actually running.
	configured := config.ResolveConvergenceMode(cfg.Convergence.Mode)
	effective := cfg.ConvergenceMode()
	_, envOverride := config.NormalizeConvergenceMode(os.Getenv(config.ConvergenceModeEnvVar))
	_, generation := s.ConvergenceModeGeneration()
	return map[string]interface{}{
		"mode":           configured,
		"effective_mode": effective,
		"env_override":   envOverride,
		"generation":     generation,
		"modes":          config.ConvergenceModes(),
	}
}
